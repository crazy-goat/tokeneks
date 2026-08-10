package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"tokeneks/compute"
)

// ocModelPrices is OpenCode's last-resort price table — see
// resolveAgentPricesSource in prices_resolve.go, which tries derived rates
// and models.dev's own "opencode" provider catalog first. This table only
// prices a model actually named one of the keys below; it must never be
// used as a blanket fallback for models it doesn't list. That fallback used
// to exist (every unpriced model silently priced at Kimi K2.6's rate) and is
// exactly what made OC's Ideal cost fiction — see prices_resolve.go.
var ocModelPrices = map[string]compute.ModelPrices{
	"Kimi K2.6": {
		Input:     0.95,
		CacheRead: 0.16,
		Output:    4.00,
	},
	"GPT 5.4 mini": {
		Input:     0.75,
		CacheRead: 0.075,
		Output:    4.50,
	},
}

// detailRowPrice is one row's own pricing info: which model actually served
// that step, the rate resolved for it (if any), and the agent's own logged
// cost to fall back on when no rate resolved. pricing[i] belongs to rows[i]
// — this is the same per-step shape sessionStep (web_store.go), ocStep and
// piSessionStep already carry, just handed to printDetailRows so it can
// price each row at its own step's rate instead of one rate for the whole
// table (see totalsByAgent in main.go, the reference implementation this
// mirrors).
//
// Sessions routinely switch models mid-run, so a single ModelPrices for
// every row — the old signature — silently priced every row that didn't
// match the table's one model at the wrong rate. Splitting the *ideal*
// computation by model to work around that would be worse: ComputeIdeal/
// ComputeIdealClaude carry state forward step to step, so cutting the
// session into per-model pieces before running them throws that state away
// at every switch (see totalsByAgent's doc comment). Only pricing is
// per-row; rows itself must still come from one continuous pass over the
// session's untouched step order.
type detailRowPrice struct {
	Model      string
	Prices     compute.ModelPrices
	Priced     bool
	LoggedCost float64
}

// uniformDetailPricing builds a detailRowPrice slice for callers that
// already split a session into single-model groups before calling
// printDetailRows (claudeDetail, piDetail): every row in such a group
// shares one model and one already-resolved price, so there is nothing to
// fall back to.
func uniformDetailPricing(n int, model string, prices compute.ModelPrices) []detailRowPrice {
	pricing := make([]detailRowPrice, n)
	for i := range pricing {
		pricing[i] = detailRowPrice{Model: model, Prices: prices, Priced: true}
	}
	return pricing
}

// detailRowCostKind says where a row's dollar figure in printDetailRows came
// from, so the table can mark it rather than let a logged-cost stand-in or a
// genuinely unknown cost look identical to a real rate-priced number.
type detailRowCostKind int

const (
	detailRowCostRated   detailRowCostKind = iota // priced from a resolved rate
	detailRowCostLogged                           // no rate; fell back to the agent's own logged cost
	detailRowCostUnknown                          // no rate and nothing logged — genuinely unknown
)

// detailRowCost resolves one row's actual dollar cost from its own pricing,
// preferring a resolved rate and falling back to the agent's logged cost —
// the same preference order totalsByAgent uses, just applied per displayed
// row instead of per aggregate.
func detailRowCost(r compute.IdealRow, p detailRowPrice) (float64, detailRowCostKind) {
	if p.Priced {
		step := compute.StepData{
			Input: r.Input, CacheCreation: r.CacheCreation, CacheCreation1h: r.CacheCreation1h,
			CacheRead: r.CacheRead, Output: r.Output,
		}
		return compute.PiStepActualCost(step, p.Prices), detailRowCostRated
	}
	if p.LoggedCost > 0 {
		return p.LoggedCost, detailRowCostLogged
	}
	return 0, detailRowCostUnknown
}

// formatDetailRowCost renders one row's $ column. A rate-priced row looks
// like a plain dollar figure; a logged-cost fallback is marked with "~" so
// it doesn't read as a number this program computed from a rate table it
// didn't actually have; a row with neither reads "n/a" rather than a
// misleading $0.0000.
func formatDetailRowCost(cost float64, kind detailRowCostKind) string {
	switch kind {
	case detailRowCostRated:
		return fmt.Sprintf("$%.4f", cost)
	case detailRowCostLogged:
		return fmt.Sprintf("$%.4f~", cost)
	default:
		return "n/a"
	}
}

// detailTotals accumulates the per-category dollar figures for the "$"
// summary line, built by summing each row's own contribution at its own
// rate rather than the old single-price-times-pooled-tokens computation.
// Rows priced from a logged-cost fallback (no rate to decompose into
// categories) are kept out of the category sums and tallied separately in
// LoggedCost/LoggedRows so the categorized figures above stay meaningful;
// rows with no price at all land in UnknownRows/UnknownTokens instead of
// silently vanishing.
type detailTotals struct {
	CRCost, CCCost, InCost, OutCost       float64
	IdealCRCost, IdealCCCost, IdealInCost float64
	WasteCost                             float64
	LoggedCost                            float64
	LoggedRows                            int
	UnknownRows                           int
	UnknownTokens                         int
}

// computeDetailRowCosts computes each row's own $ figure and kind, plus the
// running totals the "$" summary line needs. Split out from the printing
// loop so the pricing logic — the fix this file exists for — can be tested
// directly against known numbers instead of scraping printed text.
func computeDetailRowCosts(rows []compute.IdealRow, pricing []detailRowPrice) ([]float64, []detailRowCostKind, detailTotals) {
	costs := make([]float64, len(rows))
	kinds := make([]detailRowCostKind, len(rows))
	var t detailTotals

	for i, r := range rows {
		var p detailRowPrice
		if i < len(pricing) {
			p = pricing[i]
		}
		cost, kind := detailRowCost(r, p)
		costs[i] = cost
		kinds[i] = kind

		switch kind {
		case detailRowCostRated:
			cc5m := r.CacheCreation - r.CacheCreation1h
			if cc5m < 0 {
				cc5m = 0
			}
			t.CRCost += float64(r.CacheRead) * p.Prices.CacheRead / compute.TokensPerMillion
			t.CCCost += float64(cc5m)*p.Prices.CacheCreation/compute.TokensPerMillion +
				float64(r.CacheCreation1h)*p.Prices.CacheCreation1h/compute.TokensPerMillion
			t.InCost += float64(r.Input) * p.Prices.Input / compute.TokensPerMillion
			t.OutCost += float64(r.Output) * p.Prices.Output / compute.TokensPerMillion
			t.IdealCRCost += float64(r.IdealCR) * p.Prices.CacheRead / compute.TokensPerMillion
			// IdealCC is a synthetic re-derivation with no real 5m/1h split,
			// so — same convention as compute.Summarize — it's priced
			// entirely at the 5m rate.
			t.IdealCCCost += float64(r.IdealCC) * p.Prices.CacheCreation / compute.TokensPerMillion
			t.IdealInCost += float64(r.IdealIn) * p.Prices.Input / compute.TokensPerMillion
			t.WasteCost += float64(r.Waste) * (p.Prices.Input - p.Prices.CacheRead) / compute.TokensPerMillion
		case detailRowCostLogged:
			t.LoggedCost += cost
			t.LoggedRows++
		case detailRowCostUnknown:
			t.UnknownRows++
			t.UnknownTokens += r.Input + r.CacheCreation + r.CacheRead + r.Output
		}
	}

	return costs, kinds, t
}

// printDetailRowFootnotes prints the legend for the "~" and "n/a" markers
// formatDetailRowCost may have used, but only the lines that are actually
// needed — a session priced cleanly at one resolvable rate throughout prints
// no footnote at all, same as before this fix.
func printDetailRowFootnotes(t detailTotals) {
	if t.LoggedRows > 0 {
		fmt.Printf("  ~ %d row(s) have no resolvable rate; $ is the agent's own logged cost ($%.4f total, excluded from the categorized $ line above)\n",
			t.LoggedRows, t.LoggedCost)
	}
	if t.UnknownRows > 0 {
		fmt.Printf("  n/a %d row(s) have no resolvable rate and no logged cost; cost unknown (%s tokens, excluded from every total above)\n",
			t.UnknownRows, formatTokens(t.UnknownTokens))
	}
}

// printDetailRows renders the illustrative per-step table for `oc detail`,
// `pi detail` and `claude detail`. It used to take one compute.ModelPrices
// and price every row with it — fine as long as a session only ever used
// one model, which sessions routinely don't. pricing[i] now carries rows[i]'s
// own model and rate (see detailRowPrice), so a session that switches models
// mid-run prices each row correctly instead of applying whichever model
// happened to be dominant (or happened to be passed in) to every row.
//
// The Model column exists because a table that keeps mixing models without
// ever naming them is the exact problem being fixed here — once rows can
// legitimately show different rates, the reader needs to see why.
func printDetailRows(rows []compute.IdealRow, pricing []detailRowPrice, showCC bool) {
	costs, kinds, t := computeDetailRowCosts(rows, pricing)

	modelFor := func(i int) string {
		if i < len(pricing) {
			return pricing[i].Model
		}
		return ""
	}

	if showCC {
		header := fmt.Sprintf("%4s  %-18s  %7s  %7s  %7s  %6s  │  %8s  %8s  %6s  │  %7s  %8s  %11s",
			"Step", "Model", "c.read", "c.write", "input", "output", "i_cr", "i_cc", "out", "waste", "note", "$")
		fmt.Println(header)
		fmt.Println(strings.Repeat("-", utf8.RuneCountInString(header)))

		for i, r := range rows {
			fmt.Printf("%4d  %-18s  %7d  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d  %8s  %11s\n",
				i+1, truncate(modelFor(i), 18), r.CacheRead, r.CacheCreation, r.Input, r.Output,
				r.IdealCR, r.IdealCC, r.Output,
				r.Waste, r.Note(), formatDetailRowCost(costs[i], kinds[i]))
		}

		fmt.Println(strings.Repeat("-", utf8.RuneCountInString(header)))
		s := compute.Summarize(rows, compute.ModelPrices{})
		fmt.Printf("%4s  %-18s  %7d  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d\n",
			"SUM", "", s.TotalCR, s.TotalCC, s.TotalIn, s.TotalOut,
			s.TotalIdealCR, s.TotalIdealCC, s.TotalOut,
			s.TotalWaste)
		fmt.Printf("%4s  %-18s  %7.2f  %7.2f  %7.2f  %6.2f  │  %8.2f  %8.2f  %6.2f  │  %7.2f\n",
			"$", "",
			t.CRCost, t.CCCost, t.InCost, t.OutCost,
			t.IdealCRCost, t.IdealCCCost, t.OutCost,
			t.WasteCost)
		printDetailRowFootnotes(t)
		return
	}

	header := fmt.Sprintf("%4s  %-18s  %7s  %7s  %6s  │  %8s  %8s  %6s  │  %7s  %8s  %11s",
		"Step", "Model", "c.read", "input", "output", "i_cr", "i_in", "out", "waste", "note", "$")
	fmt.Println(header)
	fmt.Println(strings.Repeat("-", utf8.RuneCountInString(header)))

	for i, r := range rows {
		fmt.Printf("%4d  %-18s  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d  %8s  %11s\n",
			i+1, truncate(modelFor(i), 18), r.CacheRead, r.Input, r.Output,
			r.IdealCR, r.IdealIn, r.Output,
			r.Waste, r.Note(), formatDetailRowCost(costs[i], kinds[i]))
	}

	fmt.Println(strings.Repeat("-", utf8.RuneCountInString(header)))
	s := compute.Summarize(rows, compute.ModelPrices{})
	fmt.Printf("%4s  %-18s  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d\n",
		"SUM", "", s.TotalCR, s.TotalIn, s.TotalOut,
		s.TotalIdealCR, s.TotalIdealIn, s.TotalOut,
		s.TotalWaste)
	fmt.Printf("%4s  %-18s  %7.2f  %7.2f  %6.2f  │  %8.2f  %8.2f  %6.2f  │  %7.2f\n",
		"$", "",
		t.CRCost, t.InCost, t.OutCost,
		t.IdealCRCost, t.IdealInCost, t.OutCost,
		t.WasteCost)
	printDetailRowFootnotes(t)
}

func printDetailRowsClaude(rows []compute.IdealRow, pricing []detailRowPrice) {
	printDetailRows(rows, pricing, true)
}
