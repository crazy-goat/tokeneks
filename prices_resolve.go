package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"tokeneks/compute"
)

// resolveWarnOut is where resolveAgentPrices' once-per-model "no price"
// warning and the ambiguous-normalization warning go. A var, not a direct
// os.Stderr call, so tests can capture it — see claudeUnknownWarnOut in
// claude.go, which this mirrors.
var resolveWarnOut io.Writer = os.Stderr

var (
	agentPriceWarnMu sync.Mutex
	agentPriceWarned = map[string]bool{} // key: agent + "\x00" + model
)

// warnNoAgentPrice prints one warning per (agent, model) per process. Both
// pi and opencode can run thousands of messages through resolveAgentPrices
// in one report, so without the dedup a single unpriced model would flood
// stderr — see warnUnknownClaudeModel in claude.go for the same problem
// solved the same way.
func warnNoAgentPrice(agent, model string) {
	agentPriceWarnMu.Lock()
	defer agentPriceWarnMu.Unlock()
	key := agent + "\x00" + model
	if agentPriceWarned[key] {
		return
	}
	agentPriceWarned[key] = true
	fmt.Fprintf(resolveWarnOut, "warning: no price for %s model %q; its tokens are excluded from cost\n", agent, model)
}

// resetAgentPriceWarnings clears the warned-once set. Test-only — without
// it, whichever test hits an unknown model first swallows the warning for
// every later test reusing the same model name.
func resetAgentPriceWarnings() {
	agentPriceWarnMu.Lock()
	defer agentPriceWarnMu.Unlock()
	agentPriceWarned = map[string]bool{}
}

// resolvedPriceTables holds every store-backed layer resolveAgentPrices
// needs, built once per process. ocDevExact and ocDevNorm both come from
// model_price rows under provider "opencode" (models.dev's own hosted-model
// router, synced by `prices update`) — exact keeps the row under its own
// models.dev id (e.g. "deepseek-v4-flash", which some OpenCode log entries
// use verbatim), normalized keeps it under a folded key (see
// normalizeModelKey) for the far more common case where OpenCode logs a
// display name like "Claude Opus 4.8" that models.dev spells
// "claude-opus-4-8".
type resolvedPriceTables struct {
	// derivedPI/derivedOC are keyed by model, each holding that model's
	// dated windows oldest-first — a derived rate can now have more than
	// one, since prices derive fits a rate change as separate time
	// segments (see prices_derive.go) — rather than a single price, unlike
	// every other layer here, which only ever states "the price right
	// now" and has no dated history to consult.
	derivedPI  map[string][]agentPriceWindow
	derivedOC  map[string][]agentPriceWindow
	ocDevExact map[string]compute.ModelPrices
	ocDevNorm  map[string]compute.ModelPrices
}

// agentPriceWindow is one dated slice of a pi/opencode model's derived
// price history — the (agent, model) analogue of claude.go's
// claudePriceWindow, needed now that a derived rate can itself have more
// than one window. From is inclusive, To is exclusive; a zero value means
// unbounded on that side, matching store.ModelPrice's EffectiveFrom/To and
// claudePriceWindow's own convention.
type agentPriceWindow struct {
	from, to int64 // ms epoch
	prices   compute.ModelPrices
}

func (w agentPriceWindow) covers(at int64) bool {
	if w.from != 0 && at < w.from {
		return false
	}
	if w.to != 0 && at >= w.to {
		return false
	}
	return true
}

// resolveAgentWindow picks the window covering at out of an ordered
// (oldest-first) list — the agent-price analogue of claude.go's
// resolveClaudeWindow.
func resolveAgentWindow(windows []agentPriceWindow, at int64) (compute.ModelPrices, bool) {
	for _, w := range windows {
		if w.covers(at) {
			return w.prices, true
		}
	}
	return compute.ModelPrices{}, false
}

var (
	resolvePricesMu   sync.Mutex
	resolvePricesOnce sync.Once
	resolvePricesTbl  resolvedPriceTables
)

// resolvedPrices returns the memoized store-backed tables, building them on
// first use. Called once per (agent, model) lookup — with ~9000 opencode
// messages in a 30-day window, hitting sqlite on every one of them would
// make `total`/`oc list` noticeably slower for no benefit, since none of
// this data changes mid-report.
func resolvedPrices() resolvedPriceTables {
	resolvePricesMu.Lock()
	defer resolvePricesMu.Unlock()
	resolvePricesOnce.Do(func() {
		resolvePricesTbl = loadResolvedPriceTables()
	})
	return resolvePricesTbl
}

// resetResolvedPrices drops the memoized tables so the next lookup rebuilds
// them from the store. Called after `prices derive` writes new derived
// rates (a later command in the same process must not keep serving the
// pre-derive table — same reasoning as resetClaudePrices in prices.go) and
// by tests that seed the store directly.
func resetResolvedPrices() {
	resolvePricesMu.Lock()
	defer resolvePricesMu.Unlock()
	resolvePricesOnce = sync.Once{}
	resolvePricesTbl = resolvedPriceTables{}
}

// storeModelPricesMap reads every row for one provider out of the store and
// converts it to a plain model->price map. Returns nil (not an error) when
// the store is unavailable or the provider has no rows — every caller here
// treats that as "this layer has nothing to say," identical to a genuine
// miss.
func storeModelPricesMap(provider string) map[string]compute.ModelPrices {
	st := getTokeneksStore()
	if st == nil {
		var err error
		st, err = openTokeneksStore()
		if err != nil {
			return nil
		}
		setTokeneksStore(st)
	}
	rows, err := st.GetModelPrices(context.Background(), provider)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make(map[string]compute.ModelPrices, len(rows))
	for _, r := range rows {
		out[r.Model] = compute.ModelPrices{
			Input:                 r.Input,
			Output:                r.Output,
			CacheRead:             r.CacheRead,
			CacheCreation:         r.CacheWrite,
			CacheCreation1h:       r.CacheWrite1h,
			SupportsCacheCreation: r.CacheWrite > 0,
		}
	}
	return out
}

// storeModelPriceWindows reads every row for one provider out of the store
// and groups them by model into ordered (oldest-first) dated windows — the
// derived layer's analogue of storeModelPricesMap. Unlike a models.dev sync
// or a live catalog, which only ever have one row per model, a derived
// model can have more than one now that prices derive fits a rate change
// as separate time segments (see prices_derive.go); collapsing them into a
// plain map (last row wins, in whatever order the store happens to return
// rows) would silently drop every window but one. Returns nil (not an
// error) under the same "this layer has nothing to say" convention
// storeModelPricesMap uses.
func storeModelPriceWindows(provider string) map[string][]agentPriceWindow {
	st := getTokeneksStore()
	if st == nil {
		var err error
		st, err = openTokeneksStore()
		if err != nil {
			return nil
		}
		setTokeneksStore(st)
	}
	rows, err := st.GetModelPrices(context.Background(), provider)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make(map[string][]agentPriceWindow)
	for _, r := range rows {
		out[r.Model] = append(out[r.Model], agentPriceWindow{
			from: r.EffectiveFrom,
			to:   r.EffectiveTo,
			prices: compute.ModelPrices{
				Input:                 r.Input,
				Output:                r.Output,
				CacheRead:             r.CacheRead,
				CacheCreation:         r.CacheWrite,
				CacheCreation1h:       r.CacheWrite1h,
				SupportsCacheCreation: r.CacheWrite > 0,
			},
		})
	}
	// GetModelPrices already orders by effective_from, but re-sorting here
	// keeps correctness independent of that ordering detail.
	for model := range out {
		windows := out[model]
		sort.Slice(windows, func(i, j int) bool { return windows[i].from < windows[j].from })
		out[model] = windows
	}
	return out
}

// normalizeModelKey folds a display name and a models.dev slug onto a
// common key: lowercase, spaces and dots to hyphens. OpenCode logs display
// names ("Claude Opus 4.8") while models.dev spells the same model as a
// slug ("claude-opus-4-8") — sometimes keeping a dot as a dot (e.g.
// "glm-5.2"), sometimes turning it into a hyphen (e.g. "claude-opus-4-8"),
// with no way to tell from the string alone which convention a given model
// uses. Normalizing both sides through the same fold sidesteps having to
// guess: it's only ever used as a candidate match, accepted at the call
// site solely when exactly one models.dev model folds to the same key —
// see loadResolvedPriceTables.
func normalizeModelKey(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '.' {
			return '-'
		}
		return r
	}, s)
}

// warnAmbiguousNormalizedModel reports a models.dev normalization collision:
// more than one model under provider "opencode" folds to the same
// normalized key, so a log entry using that key cannot be resolved through
// the normalized layer without guessing which one it meant. Left unresolved
// (falling through to the next layer) rather than picking one — a wrong
// pick would look exactly as authoritative as a real match.
func warnAmbiguousNormalizedModel(key string, models []string) {
	fmt.Fprintf(resolveWarnOut, "warning: ambiguous models.dev normalization %q matches %s; skipping the normalized match for these\n",
		key, strings.Join(models, ", "))
}

// loadResolvedPriceTables builds every store-backed layer resolveAgentPrices
// reads, in one pass. Caller must hold resolvePricesMu (via resolvedPrices'
// sync.Once).
func loadResolvedPriceTables() resolvedPriceTables {
	var tbl resolvedPriceTables
	tbl.derivedPI = storeModelPriceWindows(derivedProvider("pi"))
	tbl.derivedOC = storeModelPriceWindows(derivedProvider("opencode"))

	st := getTokeneksStore()
	if st == nil {
		var err error
		st, err = openTokeneksStore()
		if err == nil {
			setTokeneksStore(st)
		}
	}
	if st == nil {
		return tbl
	}
	rows, err := st.GetModelPrices(context.Background(), "opencode")
	if err != nil || len(rows) == 0 {
		return tbl
	}

	tbl.ocDevExact = make(map[string]compute.ModelPrices, len(rows))
	normGroups := make(map[string][]string, len(rows))
	for _, r := range rows {
		p := compute.ModelPrices{
			Input:                 r.Input,
			Output:                r.Output,
			CacheRead:             r.CacheRead,
			CacheCreation:         r.CacheWrite,
			CacheCreation1h:       r.CacheWrite1h,
			SupportsCacheCreation: r.CacheWrite > 0,
		}
		tbl.ocDevExact[r.Model] = p
		key := normalizeModelKey(r.Model)
		normGroups[key] = append(normGroups[key], r.Model)
	}

	tbl.ocDevNorm = make(map[string]compute.ModelPrices, len(normGroups))
	for key, models := range normGroups {
		if len(models) == 1 {
			tbl.ocDevNorm[key] = tbl.ocDevExact[models[0]]
			continue
		}
		sort.Strings(models)
		warnAmbiguousNormalizedModel(key, models)
	}
	return tbl
}

// resolveAgentPricesAtSource resolves model's price for agent as of "at"
// (ms epoch) and reports which layer answered, so `prices check` can say
// exactly where a price came from rather than just whether one exists.
//
// Precedence for pi: derived:pi (fitted from pi's own logged costs) beats
// ~/.pi/agent/models.json (the live catalog). This is deliberate, not an
// accident of implementation order: a derived rate is measured from the
// actual bills for the traffic being reported, while the live catalog only
// ever states today's price — which may not be what that traffic was
// billed at, especially for a model the catalog has since dropped
// entirely. There's a useful property that falls out of this for free: if
// a model's price changed mid-window, `prices derive`'s change-point
// detection (see detectSegments in prices_derive.go) splits the window
// into segments that each admit one stable rate, and the residual gate
// rejects any segment that still doesn't — meaning a derived window that
// *did* survive is itself evidence the rate was stable across that
// specific span, not just a guess that happened to land close.
//
// Precedence for opencode: derived:opencode, then models.dev's own
// "opencode" provider (exact model-id match, then a normalized-key match —
// see normalizeModelKey), then the small built-in ocModelPrices table.
// There is deliberately no blanket fallback to any one model's rate for
// every other unpriced model — that was the actual bug being fixed here.
//
// Only the derived layer is dated (see agentPriceWindow) — the catalog,
// models.dev and builtin layers only ever state "the price right now" and
// have no history to resolve at, so a message whose timestamp falls
// outside every derived window (a gap between segments, or a model with no
// accepted segment at all) falls through to them exactly as it would with
// no timestamp at all.
func resolveAgentPricesAtSource(agent, model string, at int64) (compute.ModelPrices, string, bool) {
	switch agent {
	case "pi":
		tbl := resolvedPrices()
		// No "&& p.Input > 0" here, unlike the plain-map layers below: a
		// derived window can now legitimately have Input pinned to exactly
		// 0 (see fitModelRates' variance pre-filter — a placeholder input
		// column with negligible token share, GPT 5.6 Sol's case), and
		// that is a real, reported answer, not a lookup miss. ok already
		// means "a window actually covers this timestamp" — a genuine
		// miss (no window, or none covering at) returns ok=false on its
		// own, so there's nothing left for an Input check to guard against.
		if p, ok := resolveAgentWindow(tbl.derivedPI[model], at); ok {
			return p, "derived", true
		}
		if p, ok := piPricesFunc()[model]; ok && p.Input > 0 {
			return p, "catalog", true
		}
		warnNoAgentPrice(agent, model)
		return compute.ModelPrices{}, "MISSING", false

	case "opencode":
		tbl := resolvedPrices()
		// See the "pi" branch above for why there's no "&& p.Input > 0"
		// here — a pinned Input of exactly 0 is a real, reported answer.
		if p, ok := resolveAgentWindow(tbl.derivedOC[model], at); ok {
			return p, "derived", true
		}
		if p, ok := tbl.ocDevExact[model]; ok && p.Input > 0 {
			return p, "models.dev", true
		}
		if p, ok := tbl.ocDevNorm[normalizeModelKey(model)]; ok && p.Input > 0 {
			return p, "models.dev(norm)", true
		}
		if p, ok := ocModelPrices[model]; ok && p.Input > 0 {
			return p, "builtin", true
		}
		warnNoAgentPrice(agent, model)
		return compute.ModelPrices{}, "MISSING", false

	default:
		return compute.ModelPrices{}, "MISSING", false
	}
}

// resolveAgentPricesAt is resolveAgentPricesAtSource for callers that only
// need the price, not which layer produced it.
func resolveAgentPricesAt(agent, model string, at int64) (compute.ModelPrices, bool) {
	p, _, ok := resolveAgentPricesAtSource(agent, model, at)
	return p, ok
}

// resolveAgentPricesSource is resolveAgentPricesAtSource resolved "now",
// for callers with no message timestamp to resolve at — the same
// resolve-at-the-current-moment convention claudeGlobalModelPrices uses for
// Claude's own dated windows.
func resolveAgentPricesSource(agent, model string) (compute.ModelPrices, string, bool) {
	return resolveAgentPricesAtSource(agent, model, time.Now().UnixMilli())
}

// resolveAgentPrices is resolveAgentPricesSource for callers that only need
// the price, not which layer produced it.
func resolveAgentPrices(agent, model string) (compute.ModelPrices, bool) {
	p, _, ok := resolveAgentPricesSource(agent, model)
	return p, ok
}
