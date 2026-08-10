package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"tokeneks/compute"
	"tokeneks/store"
)

// captureResolveWarnings points resolveWarnOut at a buffer for the duration
// of one test and clears the warned-once set, mirroring
// claudeUnknownWarnOut's test setup in claude_prices_test.go.
func captureResolveWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	resetAgentPriceWarnings()
	prev := resolveWarnOut
	var buf bytes.Buffer
	resolveWarnOut = &buf
	t.Cleanup(func() { resolveWarnOut = prev })
	return &buf
}

// A derived rate must win over the live ~/.pi/agent/models.json catalog —
// it's measured from this traffic's own bills, while the catalog only ever
// states today's price. This is also PI's only route back to a price for a
// model the catalog has since dropped (e.g. a retired "GPT 5.4").
func TestResolveAgentPrices_PIDerivedBeatsCatalog(t *testing.T) {
	st := withTempStore(t)

	prevPrices := piPricesFunc
	piPricesFunc = func() map[string]compute.ModelPrices {
		return map[string]compute.ModelPrices{"test-model": {Input: 1.0, Output: 2.0}}
	}
	t.Cleanup(func() { piPricesFunc = prevPrices })

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: derivedProvider("pi"), Model: "test-model", Input: 9.0, Output: 18.0, Source: priceSourceDerived, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	p, ok := resolveAgentPrices("pi", "test-model")
	if !ok {
		t.Fatal("expected a price")
	}
	if p.Input != 9.0 {
		t.Errorf("Input = %v, want 9.0 (derived, not the catalog's 1.0)", p.Input)
	}
}

// PI falls back to the live catalog for a model derive has nothing to say
// about.
func TestResolveAgentPrices_PIFallsBackToCatalog(t *testing.T) {
	withTempStore(t)

	prevPrices := piPricesFunc
	piPricesFunc = func() map[string]compute.ModelPrices {
		return map[string]compute.ModelPrices{"catalog-only-model": {Input: 3.0, Output: 6.0}}
	}
	t.Cleanup(func() { piPricesFunc = prevPrices })

	p, ok := resolveAgentPrices("pi", "catalog-only-model")
	if !ok {
		t.Fatal("expected a price from the catalog layer")
	}
	if p.Input != 3.0 {
		t.Errorf("Input = %v, want 3.0", p.Input)
	}
}

// opencode's precedence: derived:opencode beats models.dev's own "opencode"
// provider rows beats the small built-in ocModelPrices table.
func TestResolveAgentPrices_OCDerivedBeatsModelsDevBeatsBuiltin(t *testing.T) {
	st := withTempStore(t)

	// Temporarily add a builtin entry for this model so all three layers are
	// exercised at once; restore the real table afterward.
	prevBuiltin := ocModelPrices
	ocModelPrices = map[string]compute.ModelPrices{"layer-test-model": {Input: 1.0, Output: 1.0}}
	t.Cleanup(func() { ocModelPrices = prevBuiltin })

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "opencode", Model: "layer-test-model", Input: 2.0, Output: 2.0, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed models.dev row: %v", err)
	}
	resetResolvedPrices()

	// With only the models.dev and builtin layers present, models.dev wins.
	p, ok := resolveAgentPrices("opencode", "layer-test-model")
	if !ok {
		t.Fatal("expected a price")
	}
	if p.Input != 2.0 {
		t.Errorf("Input = %v, want 2.0 (models.dev over builtin)", p.Input)
	}

	// Adding a derived row must now win over both.
	err = st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: derivedProvider("opencode"), Model: "layer-test-model", Input: 3.0, Output: 3.0, Source: priceSourceDerived, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed derived row: %v", err)
	}
	resetResolvedPrices()

	p, ok = resolveAgentPrices("opencode", "layer-test-model")
	if !ok {
		t.Fatal("expected a price")
	}
	if p.Input != 3.0 {
		t.Errorf("Input = %v, want 3.0 (derived over models.dev and builtin)", p.Input)
	}
}

// The removed OC fallback: a model absent from every layer must not come
// back priced at Kimi K2.6's rate (or any other model's rate) — it must
// report a genuine miss. This was the actual bug: eleven unrelated OC
// models were all silently priced at a twelfth model's rate.
func TestResolveAgentPrices_OCUnresolvedModelIsNotKimiPriced(t *testing.T) {
	withTempStore(t)
	captureResolveWarnings(t)

	p, ok := resolveAgentPrices("opencode", "totally-unknown-model")
	if ok {
		t.Fatalf("expected ok=false for an unresolvable model, got %+v", p)
	}
	if p != (compute.ModelPrices{}) {
		t.Errorf("expected the zero value, got %+v", p)
	}
	if p == ocModelPrices["Kimi K2.6"] {
		t.Fatal("test fixture coincidentally matches Kimi K2.6's rate and proves nothing")
	}
}

// A model missing from every pi layer must likewise report a genuine miss,
// not a silent zero-cost price.
func TestResolveAgentPrices_PIUnresolvedModelIsMiss(t *testing.T) {
	withTempStore(t)
	captureResolveWarnings(t)

	prevPrices := piPricesFunc
	piPricesFunc = func() map[string]compute.ModelPrices { return map[string]compute.ModelPrices{} }
	t.Cleanup(func() { piPricesFunc = prevPrices })

	p, ok := resolveAgentPrices("pi", "totally-unknown-pi-model")
	if ok {
		t.Fatalf("expected ok=false, got %+v", p)
	}
	if p != (compute.ModelPrices{}) {
		t.Errorf("expected the zero value, got %+v", p)
	}
}

// A repeated lookup of the same unresolvable model must warn exactly once
// per process, not once per call — total/oc list can run this path
// thousands of times in one report.
func TestWarnNoAgentPrice_WarnsOnce(t *testing.T) {
	withTempStore(t)
	buf := captureResolveWarnings(t)

	for i := 0; i < 5; i++ {
		if _, ok := resolveAgentPrices("opencode", "repeat-unknown-model"); ok {
			t.Fatal("expected ok=false")
		}
	}
	got := strings.Count(buf.String(), "repeat-unknown-model")
	if got != 1 {
		t.Errorf("warning printed %d time(s), want 1: %s", got, buf.String())
	}

	// A different unknown model still gets its own warning.
	resolveAgentPrices("opencode", "another-unknown-model")
	if strings.Count(buf.String(), "another-unknown-model") != 1 {
		t.Errorf("second model's warning missing: %s", buf.String())
	}
}

// OpenCode logs display names ("Claude Opus 4.8") while models.dev spells
// the same model as a slug ("claude-opus-4-8"); the normalized match is
// what bridges them, and this is the real case that motivated it (see
// prices_resolve.go's SharePct comment for why derive alone can't fix it —
// derive rejects this exact model as unidentified).
func TestResolveAgentPrices_OCNormalizedMatch(t *testing.T) {
	st := withTempStore(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "opencode", Model: "claude-opus-4-8", Input: 5.0, Output: 25.0, CacheRead: 0.5, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	p, ok := resolveAgentPrices("opencode", "Claude Opus 4.8")
	if !ok {
		t.Fatal("expected the normalized match to resolve")
	}
	if p.Input != 5.0 || p.Output != 25.0 {
		t.Errorf("got %+v, want the claude-opus-4-8 row (Input=5, Output=25)", p)
	}
}

// A models.dev model id matched exactly (no normalization needed) must
// resolve directly — some OpenCode log entries use the raw provider slug
// verbatim (e.g. "deepseek-v4-flash") rather than a display name.
func TestResolveAgentPrices_OCExactModelsDevMatch(t *testing.T) {
	st := withTempStore(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "opencode", Model: "deepseek-v4-flash", Input: 0.14, Output: 0.28, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	p, ok := resolveAgentPrices("opencode", "deepseek-v4-flash")
	if !ok {
		t.Fatal("expected an exact match")
	}
	if p.Input != 0.14 {
		t.Errorf("Input = %v, want 0.14", p.Input)
	}
}

// Two models.dev models that fold to the same normalized key must not let
// either one win by chance — a wrong pick would look exactly as
// authoritative as a real match, so both are left unresolved and the
// collision is reported instead.
func TestResolveAgentPrices_OCAmbiguousNormalizationNotPicked(t *testing.T) {
	st := withTempStore(t)
	buf := captureResolveWarnings(t)

	// "foo-1" and "foo.1" both fold to "foo-1" under normalizeModelKey
	// (lowercase, dots/spaces to hyphens).
	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "opencode", Model: "foo-1", Input: 1.0, Output: 1.0, Source: priceSourceModelsDev, UpdatedAt: 1},
		{Provider: "opencode", Model: "foo.1", Input: 2.0, Output: 2.0, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	// A log entry that folds to the same key but doesn't exactly match
	// either seeded row must not resolve through the normalized layer.
	p, ok := resolveAgentPrices("opencode", "Foo 1")
	if ok {
		t.Fatalf("expected the ambiguous normalization to stay unresolved, got %+v", p)
	}
	if !strings.Contains(buf.String(), "ambiguous") {
		t.Errorf("expected an ambiguity warning, got: %s", buf.String())
	}

	// The exact matches must still resolve — the ambiguity is scoped to the
	// normalized layer, not to the rows themselves.
	if p, ok := resolveAgentPrices("opencode", "foo-1"); !ok || p.Input != 1.0 {
		t.Errorf("exact match for foo-1 = %+v, ok=%v, want Input=1.0", p, ok)
	}
	if p, ok := resolveAgentPrices("opencode", "foo.1"); !ok || p.Input != 2.0 {
		t.Errorf("exact match for foo.1 = %+v, ok=%v, want Input=2.0", p, ok)
	}
}

// The store-backed tables are read once per process, not once per lookup:
// a row inserted after the first resolution must not be visible until
// resetResolvedPrices runs, which is the same memoize-then-explicitly-
// invalidate contract claudeStoreOverlayPrices uses (see prices_test.go's
// TestClaudePrices_StoreBeatsBuiltinDefault).
func TestResolvedPrices_MemoizedUntilReset(t *testing.T) {
	st := withTempStore(t)

	// Establish the memo with no rows present.
	if _, ok := resolveAgentPrices("pi", "late-model"); ok {
		t.Fatal("expected a miss before the row exists")
	}

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: derivedProvider("pi"), Model: "late-model", Input: 4.0, Output: 4.0, Source: priceSourceDerived, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, ok := resolveAgentPrices("pi", "late-model"); ok {
		t.Fatal("expected the stale memo to still report a miss before reset")
	}

	resetResolvedPrices()

	p, ok := resolveAgentPrices("pi", "late-model")
	if !ok {
		t.Fatal("expected the row to be visible after reset")
	}
	if p.Input != 4.0 {
		t.Errorf("Input = %v, want 4.0", p.Input)
	}
}

// `prices derive` writing new rates must invalidate the resolver's memo, or
// a later command in the same process keeps serving the pre-derive table —
// the exact failure mode resetClaudePrices exists to prevent for Claude
// (see prices.go's runPricesUpdate). deriveModelRates is called directly
// (not runPricesDerive) for the same reason prices_derive_test.go does:
// runPricesDerive goes through ensureStoreReady's real filesystem sync,
// which this test must not touch.
func TestDeriveModelRates_InvalidatesResolvedPricesMemo(t *testing.T) {
	st := withDeriveStore(t)

	// Prime the memo with a miss before any data exists.
	if _, ok := resolveAgentPrices("pi", "flaky-model"); ok {
		t.Fatal("expected a miss before deriving")
	}

	trueRates := compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4}
	ingestTotalTestSession(t, st, "pi", "s1", goodModelSteps("flaky-model", trueRates))

	if _, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false); err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}

	p, ok := resolveAgentPrices("pi", "flaky-model")
	if !ok {
		t.Fatal("expected the newly derived rate to be visible without an explicit reset in the test")
	}
	// The fit goes through floating-point NNLS, so compare with a tolerance
	// rather than exact equality (see approxEqual in prices_derive_test.go).
	if !approxEqual(p.Input, trueRates.Input, 1e-9) {
		t.Errorf("Input = %v, want %v", p.Input, trueRates.Input)
	}
}

// A model with two derived windows (a rate that changed mid-window, see
// prices_derive.go's segmentation) must resolve a message from before the
// boundary to the earlier rate and one from after it to the later rate —
// the whole point of dating the rows at all.
func TestResolveAgentPricesAt_TwoWindowsResolveByTimestamp(t *testing.T) {
	st := withTempStore(t)

	const boundary = int64(1_700_000_000_000)
	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: derivedProvider("opencode"), Model: "GPT 5.6 Luna", Input: 5.0, Output: 25.0, Source: priceSourceDerived, UpdatedAt: 1, EffectiveFrom: 0, EffectiveTo: boundary},
		{Provider: derivedProvider("opencode"), Model: "GPT 5.6 Luna", Input: 1.1, Output: 6.6, Source: priceSourceDerived, UpdatedAt: 1, EffectiveFrom: boundary, EffectiveTo: 0},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	before, ok := resolveAgentPricesAt("opencode", "GPT 5.6 Luna", boundary-1)
	if !ok || before.Input != 5.0 {
		t.Errorf("before boundary: got %+v, ok=%v, want the pre-shift window (Input=5.0)", before, ok)
	}
	at, ok := resolveAgentPricesAt("opencode", "GPT 5.6 Luna", boundary)
	if !ok || at.Input != 1.1 {
		t.Errorf("at boundary (To is exclusive, From is inclusive): got %+v, ok=%v, want the post-shift window (Input=1.1)", at, ok)
	}
	after, ok := resolveAgentPricesAt("opencode", "GPT 5.6 Luna", boundary+1_000_000)
	if !ok || after.Input != 1.1 {
		t.Errorf("after boundary: got %+v, ok=%v, want the post-shift window (Input=1.1)", after, ok)
	}
}

// A model with a single, fully open derived window (the ordinary case —
// no rate change ever detected) must resolve to the same price at any
// timestamp, including one far outside any window a real report would
// ever query — there's nothing dated about it to get wrong.
func TestResolveAgentPricesAt_SingleOpenWindowResolvesAtAnyTime(t *testing.T) {
	st := withTempStore(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: derivedProvider("pi"), Model: "GLM 5.2", Input: 1.4, CacheRead: 0.26, Output: 4.4, Source: priceSourceDerived, UpdatedAt: 1, EffectiveFrom: 0, EffectiveTo: 0},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	for _, at := range []int64{0, 1, 1_700_000_000_000, 9_999_999_999_999} {
		p, ok := resolveAgentPricesAt("pi", "GLM 5.2", at)
		if !ok || p.Input != 1.4 {
			t.Errorf("at=%d: got %+v, ok=%v, want Input=1.4 at every timestamp", at, p, ok)
		}
	}
}

// resolveAgentPricesSource (the no-timestamp caller) must resolve "now" —
// so a message-less caller sees the currently-open window of a model that
// has more than one, not a stale earlier one and not a miss.
func TestResolveAgentPrices_NoTimestampResolvesNow(t *testing.T) {
	st := withTempStore(t)

	past := time.Now().Add(-24 * time.Hour).UnixMilli()
	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: derivedProvider("opencode"), Model: "GPT 5.6 Luna", Input: 5.0, Output: 25.0, Source: priceSourceDerived, UpdatedAt: 1, EffectiveFrom: 0, EffectiveTo: past},
		{Provider: derivedProvider("opencode"), Model: "GPT 5.6 Luna", Input: 1.1, Output: 6.6, Source: priceSourceDerived, UpdatedAt: 1, EffectiveFrom: past, EffectiveTo: 0},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	p, ok := resolveAgentPrices("opencode", "GPT 5.6 Luna")
	if !ok || p.Input != 1.1 {
		t.Errorf("got %+v, ok=%v, want the currently-open window (Input=1.1), not the closed pre-shift one", p, ok)
	}
}

// A historical lookup (resolveAgentPricesAt with a real past timestamp) that
// resolves through an undated layer — models.dev here — must warn: that
// layer only ever states today's rate, and using it for a message from long
// ago is exactly the silent anachronism that masked GPT 5.6 Luna's gap.
func TestWarnAnachronisticCatalogPrice_FiresForHistoricalLookup(t *testing.T) {
	st := withTempStore(t)
	buf := captureResolveWarnings(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "opencode", Model: "anach-model", Input: 1.0, Output: 1.0, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	old := time.Now().Add(-90 * 24 * time.Hour).UnixMilli()
	p, ok := resolveAgentPricesAt("opencode", "anach-model", old)
	if !ok || p.Input != 1.0 {
		t.Fatalf("got %+v, ok=%v, want the models.dev row to still resolve", p, ok)
	}
	if !strings.Contains(buf.String(), "anach-model") || !strings.Contains(buf.String(), "models.dev") {
		t.Errorf("expected an anachronism warning naming the model and source, got: %s", buf.String())
	}
}

// resolveAgentPricesSource's own "now" lookup must never trigger the
// anachronism warning: it is deliberately asking for today's rate from an
// undated layer and getting exactly that, not being fooled into thinking a
// stale rate applies to an old message.
func TestWarnAnachronisticCatalogPrice_SilentForNowLookup(t *testing.T) {
	st := withTempStore(t)
	buf := captureResolveWarnings(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "opencode", Model: "now-model", Input: 1.0, Output: 1.0, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	if _, ok := resolveAgentPrices("opencode", "now-model"); !ok {
		t.Fatal("expected the models.dev row to resolve")
	}
	if buf.String() != "" {
		t.Errorf("expected no anachronism warning for a 'now' lookup, got: %s", buf.String())
	}
}

// A historical lookup that resolves through the derived layer must not warn
// — derived rates are dated (see agentPriceWindow) and are exactly the
// mechanism this warning exists to push people toward, not a case of it.
func TestWarnAnachronisticCatalogPrice_SilentForDerivedSource(t *testing.T) {
	st := withTempStore(t)
	buf := captureResolveWarnings(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: derivedProvider("opencode"), Model: "derived-model", Input: 1.0, Output: 1.0, Source: priceSourceDerived, UpdatedAt: 1, EffectiveFrom: 0, EffectiveTo: 0},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	old := time.Now().Add(-90 * 24 * time.Hour).UnixMilli()
	if _, ok := resolveAgentPricesAt("opencode", "derived-model", old); !ok {
		t.Fatal("expected the derived row to resolve")
	}
	if buf.String() != "" {
		t.Errorf("expected no anachronism warning for a derived-source resolution, got: %s", buf.String())
	}
}

// A repeated historical lookup of the same (agent, model, source) must warn
// exactly once per process — `total`/`prices check` can run this path once
// per message, and a single stale model must not flood stderr.
func TestWarnAnachronisticCatalogPrice_WarnsOncePerKey(t *testing.T) {
	st := withTempStore(t)
	buf := captureResolveWarnings(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "opencode", Model: "repeat-anach-model", Input: 1.0, Output: 1.0, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	resetResolvedPrices()

	old := time.Now().Add(-90 * 24 * time.Hour).UnixMilli()
	for i := 0; i < 5; i++ {
		if _, ok := resolveAgentPricesAt("opencode", "repeat-anach-model", old); !ok {
			t.Fatal("expected a price")
		}
	}
	if got := strings.Count(buf.String(), "repeat-anach-model"); got != 1 {
		t.Errorf("warning printed %d time(s), want 1: %s", got, buf.String())
	}
}

// End-to-end regression for the real case that motivated
// warnAnachronisticCatalogPrice: opencode's "GPT 5.6 Sol" has only 37
// logged-cost messages ever (its entire lifetime, all within one six-minute
// window), and its cache_write column — real token volume, but a true rate
// small enough that its dollar contribution to the fit lands at ~0.01% of
// the total, nowhere near minColumnSharePct's 1% floor — correctly fails
// the identifiability gate and rejects the whole segment (see
// TestDeriveModelRates_RejectsUnidentifiedColumn for the mechanism itself).
// With no derived rate stored, a historical lookup for that model falls
// through to models.dev, and must now warn about it rather than silently
// trusting an undated catalog rate for a message from long ago — that
// silent trust is what let this exact model's Ideal cost be ~60% off with
// nothing in the output to say why.
func TestResolveAgentPricesAt_UnidentifiedColumnFallsBackToModelsDevWithWarning(t *testing.T) {
	st := withDeriveStore(t)
	buf := captureResolveWarnings(t)

	// cache_read carries the bulk of the fitted cost; cache_write varies
	// for real (so it's not pre-filter pinned) but its true rate is tiny
	// next to cache_read and output — the same shape measured for GPT 5.6
	// Sol's real data (cache_write share came out at 0.013%, not a
	// rounding artifact sitting at the 1% boundary).
	const trueCacheRead, trueCacheWrite, trueOutput = 0.55, 0.001, 34.6
	type tok struct{ cr, cw, out int }
	toks := []tok{
		{10948, 200, 19}, {2369, 300, 102}, {4947, 150, 103}, {607, 900, 165},
		{229, 100, 231}, {10607, 700, 113}, {323, 2000, 287}, {10072, 50, 2014},
		{2170, 900, 172}, {8188, 250, 136}, {360, 1500, 62}, {371, 400, 186},
		{8485, 1800, 61}, {186, 600, 194}, {491, 1100, 1573}, {1892, 220, 496},
		{527, 1700, 145}, {613, 90, 193}, {275, 980, 110}, {45, 310, 324},
		{480, 1600, 86}, {96, 470, 170}, {50, 130, 160}, {1, 90, 112},
		{455, 200, 19}, {130, 300, 393}, {15577, 150, 131}, {1, 900, 233},
		{10823, 250, 217}, {386, 1500, 381}, {12643, 400, 99}, {128, 1800, 84},
		{99, 600, 178}, {790, 1100, 82}, {769, 220, 269}, {1731, 1700, 140}, {187, 90, 33},
	}
	startMs := time.Now().Add(-90 * 24 * time.Hour).UnixMilli()
	var msgs []deriveMsg
	for i, tk := range toks {
		cost := float64(tk.cr)*trueCacheRead/compute.TokensPerMillion +
			float64(tk.cw)*trueCacheWrite/compute.TokensPerMillion +
			float64(tk.out)*trueOutput/compute.TokensPerMillion
		msgs = append(msgs, deriveMsg{CacheRead: tk.cr, CacheWrite: tk.cw, Output: tk.out, Cost: cost, CreatedAt: startMs + int64(i)*1000})
	}
	seedDeriveMessages(t, st, "opencode", "collinear-cache-model", msgs)

	results, err := deriveModelRates(context.Background(), st, 3650, 20, 35.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 1 || !strings.HasPrefix(results[0].Status, "rejected: unidentified") {
		t.Fatalf("results = %+v, want a single rejected-unidentified entry (cache_write's share %.4f%%)",
			results, results[0].Fit.Columns[2].SharePct)
	}
	if prices, err := st.GetModelPrices(context.Background(), derivedProvider("opencode")); err != nil || len(prices) != 0 {
		t.Fatalf("an unidentified fit must not be stored, even partially: prices=%+v err=%v", prices, err)
	}

	// With nothing derived, seed a models.dev row (as `prices update` would
	// have synced) and resolve at the messages' own, historical timestamp.
	if err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "opencode", Model: "collinear-cache-model", Input: 1.0, Output: 50.0, Source: priceSourceModelsDev, UpdatedAt: 1},
	}); err != nil {
		t.Fatalf("seed models.dev: %v", err)
	}
	resetResolvedPrices()

	p, ok := resolveAgentPricesAt("opencode", "collinear-cache-model", startMs)
	if !ok || p.Output != 50.0 {
		t.Fatalf("got %+v, ok=%v, want the models.dev fallback (Output=50.0)", p, ok)
	}
	if !strings.Contains(buf.String(), "collinear-cache-model") || !strings.Contains(buf.String(), "models.dev") {
		t.Errorf("expected an anachronism warning naming the model and the models.dev source, got: %s", buf.String())
	}
}

// The dated derived layer is memoized the same way the rest of
// resolvedPrices is (see TestResolvedPrices_MemoizedUntilReset): a window
// inserted after the first lookup must not become visible until
// resetResolvedPrices runs, whether the caller supplies a timestamp or not.
func TestResolveAgentPricesAt_MemoizedUntilReset(t *testing.T) {
	st := withTempStore(t)

	const at = int64(1_700_000_000_000)
	if _, ok := resolveAgentPricesAt("opencode", "late-dated-model", at); ok {
		t.Fatal("expected a miss before the row exists")
	}

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: derivedProvider("opencode"), Model: "late-dated-model", Input: 9.0, Output: 9.0, Source: priceSourceDerived, UpdatedAt: 1, EffectiveFrom: 0, EffectiveTo: 0},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, ok := resolveAgentPricesAt("opencode", "late-dated-model", at); ok {
		t.Fatal("expected the stale memo to still report a miss before reset")
	}

	resetResolvedPrices()

	p, ok := resolveAgentPricesAt("opencode", "late-dated-model", at)
	if !ok || p.Input != 9.0 {
		t.Errorf("got %+v, ok=%v, want the row visible after reset (Input=9.0)", p, ok)
	}
}
