package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"tokeneks/compute"
)

// checkRow is one (agent, model) group in `prices check`: what the agent's
// own log says these messages cost, versus what tokeneks would compute from
// its own rate table for the same tokens.
type checkRow struct {
	Agent      string
	Model      string
	Msgs       int
	LoggedCost float64
	Tokens     int // input + cache_read + cache_write + output, for the unpriced summary

	// RateSource describes which table answered: "exact", "fallback(...)",
	// or "MISSING". Empty ComputedCost/HasComputed means no rate was found.
	RateSource   string
	ComputedCost float64
	HasComputed  bool

	// Circular is true for the claude agent: message.cost there is
	// tokeneks' own ingest-time recomputation (compute.PiStepActualCost),
	// not a provider-billed figure, so comparing ComputedCost against it
	// would just be comparing this program against itself.
	Circular bool
}

// resolveCheckRate looks up (agent, model) "now" the same way the rest of
// the CLI's no-timestamp callers do — resolveAgentPricesSource for
// pi/opencode, claudeGlobalModelPrices for claude — and reports which layer
// answered. Used only for claude here; pi/opencode go through
// resolveAgentPricesAtSource directly in gatherCheckRows, at each message's
// own timestamp, because a derived rate can now have more than one
// effective window (see prices_derive.go) and resolving "now" for every
// message would price a message from before a rate change at the rate
// that came after it — exactly the gap this function's dateless resolution
// can't close, which is why `prices check` doesn't call it for those two.
func resolveCheckRate(agent, model string) (compute.ModelPrices, string, bool) {
	switch agent {
	case "opencode", "pi":
		return resolveAgentPricesSource(agent, model)
	case "claude":
		if p, ok := claudeGlobalModelPrices()[model]; ok && p.Input > 0 {
			return p, "exact", true
		}
		return compute.ModelPrices{}, "MISSING", false
	default:
		return compute.ModelPrices{}, "MISSING", false
	}
}

// dominantCheckSource picks the source label to show for a (agent, model)
// row that may have been resolved through more than one layer across its
// messages — e.g. most of a derived model's messages resolve through its
// segments, but a handful in a gap between segments fall back to
// models.dev. Picks whichever layer accounts for the most resolved dollars
// (ties broken by source name, for a deterministic report); "MISSING" for
// a row where nothing resolved at all.
func dominantCheckSource(costBySource map[string]float64) string {
	sources := make([]string, 0, len(costBySource))
	for s := range costBySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	best, bestCost := "MISSING", -1.0
	for _, s := range sources {
		if c := costBySource[s]; c > bestCost {
			best, bestCost = s, c
		}
	}
	return best
}

// gatherCheckRows queries every (agent, model) that logged a nonzero cost
// within the days window and resolves its rate.
//
// pi and opencode are resolved per message, at that message's own
// timestamp: a derived rate can now have more than one effective window
// (see prices_derive.go), so aggregating tokens with SQL first and
// resolving one rate for the whole group — the old approach, still used
// for claude below — would apply whichever window happens to answer for
// the group as a whole to every message in it, including ones from a
// different window or from before any derived rate existed at all. This is
// the measuring stick for the resolver itself: if this doesn't go through
// the same per-message path `total` now uses, `prices check` would keep
// reporting stale coverage while the report it's supposed to measure
// already improved.
//
// claude keeps the old SQL-aggregate path: its message.cost is tokeneks'
// own recomputation (Circular below), and claudeGlobalModelPrices resolves
// "now" for every caller that isn't already dated (see claude.go) — there
// is no new dated derived layer to fall behind here, so per-message
// resolution would cost more without changing a single number.
func gatherCheckRows(ctx context.Context, days int) ([]checkRow, error) {
	st := getTokeneksStore()
	if st == nil {
		return nil, fmt.Errorf("store not open")
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	var out []checkRow

	msgRows, err := st.DB().QueryContext(ctx, `
		SELECT agent, model, created_at, input_tokens, cache_read, cache_write, cache_write_1h, output_tokens, cost
		FROM message
		WHERE role = 'assistant' AND agent IN ('pi', 'opencode')
		  AND model IS NOT NULL AND model != '' AND created_at >= ?
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer msgRows.Close()

	type key struct{ agent, model string }
	grouped := map[key]*checkRow{}
	costBySource := map[key]map[string]float64{}
	var order []key

	for msgRows.Next() {
		var agent, model string
		var createdAt int64
		var in, cr, cw, cw1h, o int
		var cost float64
		if err := msgRows.Scan(&agent, &model, &createdAt, &in, &cr, &cw, &cw1h, &o, &cost); err != nil {
			return nil, err
		}

		k := key{agent, model}
		r, ok := grouped[k]
		if !ok {
			r = &checkRow{Agent: agent, Model: model}
			grouped[k] = r
			costBySource[k] = map[string]float64{}
			order = append(order, k)
		}
		r.Msgs++
		r.Tokens += in + cr + cw + o
		r.LoggedCost += cost

		prices, source, ok := resolveAgentPricesAtSource(agent, model, createdAt)
		if ok {
			r.HasComputed = true
			c := compute.PiStepActualCost(compute.StepData{
				Input: in, CacheCreation: cw, CacheCreation1h: cw1h, CacheRead: cr, Output: o,
			}, prices)
			r.ComputedCost += c
			costBySource[k][source] += c
		}
	}
	if err := msgRows.Err(); err != nil {
		return nil, err
	}

	for _, k := range order {
		r := grouped[k]
		if r.LoggedCost <= 0 {
			continue // matches the pre-existing HAVING SUM(cost) > 0
		}
		r.RateSource = dominantCheckSource(costBySource[k])
		out = append(out, *r)
	}

	claudeRows, err := st.DB().QueryContext(ctx, `
		SELECT model, COUNT(*),
		       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(cache_read), 0),
		       COALESCE(SUM(cache_write), 0), COALESCE(SUM(cache_write_1h), 0),
		       COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost), 0)
		FROM message
		WHERE role = 'assistant' AND agent = 'claude'
		  AND model IS NOT NULL AND model != '' AND created_at >= ?
		GROUP BY model
		HAVING SUM(cost) > 0
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer claudeRows.Close()

	for claudeRows.Next() {
		r := checkRow{Agent: "claude", Circular: true}
		var in, cr, cw, cw1h, o int
		if err := claudeRows.Scan(&r.Model, &r.Msgs, &in, &cr, &cw, &cw1h, &o, &r.LoggedCost); err != nil {
			return nil, err
		}
		r.Tokens = in + cr + cw + o

		prices, source, ok := resolveCheckRate("claude", r.Model)
		r.RateSource = source
		if ok {
			r.HasComputed = true
			r.ComputedCost = compute.PiStepActualCost(compute.StepData{
				Input: in, CacheCreation: cw, CacheCreation1h: cw1h, CacheRead: cr, Output: o,
			}, prices)
		}
		out = append(out, r)
	}
	if err := claudeRows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].LoggedCost > out[j].LoggedCost })
	return out, nil
}

// runPricesCheck is the measuring stick: for every (agent, model) with a
// logged cost, compare that logged figure against what tokeneks' own rate
// table would compute. It never fails — it's a report, not a gate.
func runPricesCheck(days int) error {
	if err := ensureStoreReady(); err != nil {
		return err
	}

	rows, err := gatherCheckRows(context.Background(), days)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no messages with a logged cost in this window")
		return nil
	}

	fmt.Printf("%-9s %-26s %6s %10s %12s %8s  %s\n",
		"agent", "model", "msgs", "logged $", "computed $", "err", "rates")

	var pricedGroups, unpricedGroups int
	var unpricedTokens int
	var unpricedCost float64

	for _, r := range rows {
		computed := "—"
		errCol := "—"
		if r.HasComputed {
			computed = fmt.Sprintf("%10.2f", r.ComputedCost)
			switch {
			case r.Circular:
				errCol = "circular"
			case r.LoggedCost > 0:
				errCol = fmt.Sprintf("%6.1f%%", (r.ComputedCost-r.LoggedCost)/r.LoggedCost*100)
			}
			pricedGroups++
		} else {
			unpricedGroups++
			unpricedTokens += r.Tokens
			unpricedCost += r.LoggedCost
		}

		fmt.Printf("%-9s %-26s %6d %10.2f %12s %8s  %s\n",
			r.Agent, truncate(r.Model, 26), r.Msgs, r.LoggedCost, computed, errCol, r.RateSource)
	}

	fmt.Println()
	total := pricedGroups + unpricedGroups
	fmt.Printf("coverage: %d/%d models priced, %d unpriced (%s tokens, $%.2f logged)\n",
		pricedGroups, total, unpricedGroups, formatTokens(unpricedTokens), unpricedCost)

	return nil
}
