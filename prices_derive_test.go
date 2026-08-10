package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"tokeneks/compute"
	"tokeneks/store"
)

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// Noise-free data generated from known rates must come back essentially
// exact — this is the payoff case from the task: PI's own rate table
// recovers models.dev has since dropped.
func TestNnlsSolve_ExactRecovery(t *testing.T) {
	trueRates := []float64{0.95, 0.16, 0.0, 4.0} // input, cache_read, cache_write, output ($/M)

	// Independently varying token counts (not all proportional to one
	// another) so the design matrix has full column rank and the true rates
	// are the unique solution, not one point on a line of equally-good fits.
	type tok struct{ in, cr, cw, out int }
	toks := []tok{
		{5000, 500, 0, 200}, {8000, 300, 0, 800}, {3000, 1200, 0, 150},
		{10000, 100, 0, 900}, {6000, 700, 0, 300}, {1000, 2000, 0, 50},
		{12000, 50, 0, 1200}, {4000, 900, 0, 400}, {7000, 250, 0, 600},
		{2000, 1500, 0, 100}, {9000, 400, 0, 700}, {1500, 1800, 0, 80},
	}

	var msgs []deriveMsg
	for _, tk := range toks {
		cost := float64(tk.in)*trueRates[0]/compute.TokensPerMillion +
			float64(tk.cr)*trueRates[1]/compute.TokensPerMillion +
			float64(tk.cw)*trueRates[2]/compute.TokensPerMillion +
			float64(tk.out)*trueRates[3]/compute.TokensPerMillion
		msgs = append(msgs, deriveMsg{Input: tk.in, CacheRead: tk.cr, CacheWrite: tk.cw, Output: tk.out, Cost: cost})
	}

	fit := fitModelRates(msgs, 0.1, 1.0)
	prices := fit.Prices()
	if !approxEqual(prices.Input, trueRates[0], 1e-9) {
		t.Errorf("Input = %v, want %v", prices.Input, trueRates[0])
	}
	if !approxEqual(prices.CacheRead, trueRates[1], 1e-9) {
		t.Errorf("CacheRead = %v, want %v", prices.CacheRead, trueRates[1])
	}
	if !approxEqual(prices.CacheCreation, trueRates[2], 1e-9) {
		t.Errorf("CacheCreation = %v, want %v", prices.CacheCreation, trueRates[2])
	}
	if !approxEqual(prices.Output, trueRates[3], 1e-9) {
		t.Errorf("Output = %v, want %v", prices.Output, trueRates[3])
	}
	if fit.ResidualPct > 1e-7 {
		t.Errorf("ResidualPct = %v, want ~0", fit.ResidualPct)
	}
	// cache_write never appears in this fixture, so it must not participate
	// (and therefore can never be flagged "unidentified" — see the
	// low-share test below for the column that should be flagged).
	if fit.Columns[2].Participates {
		t.Error("cache_write Participates = true, want false (all-zero in this fixture)")
	}
	if bad := fit.unidentifiedColumns(1.0); len(bad) != 0 {
		t.Errorf("unidentifiedColumns = %v, want none — every participating column here carries real weight", bad)
	}
}

// Data whose unconstrained least-squares solution has a negative
// coefficient must come back with that coefficient pinned to 0 and the
// remaining ones re-fitted — never reported as a negative "price".
//
// y = 2*x0 - x1 is an exact (noise-free), well-determined relationship
// with a negative true coefficient on x1, so unconstrained OLS recovers
// exactly [2, -1]. NNLS must reject that, drop x1, and refit x0 alone.
func TestNnlsSolve_NonNegativity(t *testing.T) {
	X := [][]float64{{1, 0}, {2, 1}, {3, 2}, {4, 1}, {2, 3}}
	y := make([]float64, len(X))
	for i, row := range X {
		y[i] = 2*row[0] - row[1]
	}

	// Sanity check on the test fixture itself: confirm the unconstrained
	// solve really does produce a negative coefficient, or this test proves
	// nothing about the non-negativity constraint.
	unconstrained := solveOLS(X, y, []int{0, 1})
	if unconstrained[1] >= 0 {
		t.Fatalf("test fixture doesn't produce a negative unconstrained coefficient: %v", unconstrained)
	}

	coef := nnlsSolve(X, y)
	if coef[1] != 0 {
		t.Errorf("coef[1] = %v, want 0 (pinned, negative in the unconstrained solve)", coef[1])
	}
	if coef[0] <= 0 {
		t.Errorf("coef[0] = %v, want > 0", coef[0])
	}

	// coef[0] should be the single-variable least-squares fit of y onto x0
	// alone: sum(x0*y) / sum(x0^2).
	var num, den float64
	for i, row := range X {
		num += row[0] * y[i]
		den += row[0] * row[0]
	}
	want := num / den
	if !approxEqual(coef[0], want, 1e-9) {
		t.Errorf("coef[0] = %v, want %v (refit on x0 alone)", coef[0], want)
	}
}

// A column that is zero in every row (e.g. a model whose messages never use
// cache-write) is unidentifiable, not merely small — the solver must pin it
// to exactly 0 rather than let numerical noise or a near-singular normal
// equation send it to NaN or +/-Inf.
func TestNnlsSolve_DegenerateColumnStaysZero(t *testing.T) {
	X := [][]float64{{1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, 0}}
	y := []float64{3, 6, 9, 12, 15} // y = 3*x0 exactly; x1 is all-zero

	coef := nnlsSolve(X, y)
	if math.IsNaN(coef[1]) || math.IsInf(coef[1], 0) {
		t.Fatalf("coef[1] = %v, want a finite 0, not NaN/Inf", coef[1])
	}
	if coef[1] != 0 {
		t.Errorf("coef[1] = %v, want exactly 0 (degenerate all-zero column)", coef[1])
	}
	if !approxEqual(coef[0], 3, 1e-9) {
		t.Errorf("coef[0] = %v, want 3", coef[0])
	}
}

// A model whose logged costs don't fit any non-negative linear rate table
// must show a large residual — that's the number deriveModelRates gates on
// to reject a bad fit rather than storing it.
func TestFitModelRates_BadDataHasLargeResidual(t *testing.T) {
	msgs := []deriveMsg{
		{Input: 1000, Output: 100, Cost: 5.00},
		{Input: 2000, Output: 100, Cost: 0.01},
		{Input: 500, Output: 5000, Cost: 3.33},
		{Input: 9000, Output: 50, Cost: 0.02},
		{Input: 100, Output: 100, Cost: 9.99},
	}
	fit := fitModelRates(msgs, 0.1, 1.0)
	if fit.ResidualPct < 5 {
		t.Errorf("ResidualPct = %v, want a large (>5%%) relative residual for data with no consistent linear fit", fit.ResidualPct)
	}
}

// The residual gate alone does not catch an unidentified coefficient: a
// column whose real dollar contribution is negligible can still fit to
// ~0% residual while its coefficient is numerically arbitrary (any value
// on the same order would fit about as well). This is exactly what
// happened in practice — opencode's real "Claude Opus 4.8" logs fit both
// input=5.00 (the true Anthropic rate) and input=0.69 to 0.00% residual,
// because input's own share of the total cost there is ~0.01%.
//
// This fixture reproduces the shape deliberately: cost is generated from a
// tiny-but-nonzero true input rate (so it's a different case from the
// all-zero-usage column above — input really is used) alongside a
// dominant cache_read/output rate, so input's fitted coefficient carries
// almost none of the money.
func TestFitModelRates_LowShareColumnFlaggedUnidentified(t *testing.T) {
	trueRates := []float64{0.0005, 0.5, 0.0, 5.0} // input, cache_read, cache_write, output
	type tok struct{ in, cr, out int }
	toks := []tok{
		{5000, 500, 200}, {8000, 300, 800}, {3000, 1200, 150}, {10000, 100, 900},
		{6000, 700, 300}, {1000, 2000, 50}, {12000, 50, 1200}, {4000, 900, 400},
		{7000, 250, 600}, {2000, 1500, 100}, {9000, 400, 700}, {1500, 1800, 80},
	}
	var msgs []deriveMsg
	for _, tk := range toks {
		cost := float64(tk.in)*trueRates[0]/compute.TokensPerMillion +
			float64(tk.cr)*trueRates[1]/compute.TokensPerMillion +
			float64(tk.out)*trueRates[3]/compute.TokensPerMillion
		msgs = append(msgs, deriveMsg{Input: tk.in, CacheRead: tk.cr, Output: tk.out, Cost: cost})
	}

	fit := fitModelRates(msgs, 0.1, 1.0)
	if fit.ResidualPct > 0.01 {
		t.Fatalf("ResidualPct = %v, want ~0 — the fixture is an exact noiseless linear system", fit.ResidualPct)
	}
	if fit.Columns[0].SharePct >= 1.0 {
		t.Fatalf("input SharePct = %v, want < 1.0 — the fixture is designed so input barely moves total cost", fit.Columns[0].SharePct)
	}

	bad := fit.unidentifiedColumns(1.0)
	if len(bad) != 1 || bad[0] != "input" {
		t.Errorf("unidentifiedColumns(1.0) = %v, want [input]", bad)
	}
}

// A well-conditioned model — every column carries a real share of the
// cost — must still pass the identifiability gate unchanged; it's not a
// blanket penalty on every fit.
func TestFitModelRates_WellConditionedModelNotFlagged(t *testing.T) {
	trueRates := []float64{1.4, 0.26, 0.0, 4.4}
	type tok struct{ in, cr, out int }
	toks := []tok{
		{5000, 500, 200}, {8000, 300, 800}, {3000, 1200, 150}, {10000, 100, 900},
		{6000, 700, 300}, {1000, 2000, 50}, {12000, 50, 1200}, {4000, 900, 400},
		{7000, 250, 600}, {2000, 1500, 100}, {9000, 400, 700}, {1500, 1800, 80},
	}
	var msgs []deriveMsg
	for _, tk := range toks {
		cost := float64(tk.in)*trueRates[0]/compute.TokensPerMillion +
			float64(tk.cr)*trueRates[1]/compute.TokensPerMillion +
			float64(tk.out)*trueRates[3]/compute.TokensPerMillion
		msgs = append(msgs, deriveMsg{Input: tk.in, CacheRead: tk.cr, Output: tk.out, Cost: cost})
	}

	fit := fitModelRates(msgs, 0.1, 1.0)
	if bad := fit.unidentifiedColumns(1.0); len(bad) != 0 {
		t.Errorf("unidentifiedColumns(1.0) = %v, want none for a well-conditioned fit; shares = %+v", bad, fit.Columns)
	}
}

// testGate is a plausibilityGate with round, easy-to-reason-about bounds
// for testing implausibleColumns directly, independent of whatever the
// real models.dev catalog happens to contain.
var testGate = plausibilityGate{MaxRatePerM: 100.0, MinCacheReadShareOfInput: 0.001, MaxInputToOutputRatio: 5.0}

// A coefficient beyond the absolute ceiling must be rejected outright —
// the case the real store surfaced (a $602/M "input" rate) that neither
// the residual gate nor the identifiability gate can see, because both
// only look at how much of the money a column explains, not what value it
// claims for a token nobody sends many of.
func TestImplausibleColumns_AbsoluteCeiling(t *testing.T) {
	fit := fitResult{Columns: [4]fitColumn{
		{Coef: 150.0, Participates: true, SharePct: 20.0}, // input — over the 100.0 ceiling
		{Coef: 0.15, Participates: true, SharePct: 70.0},  // cache_read
		{}, // cache_write — doesn't participate
		{Coef: 40.0, Participates: true, SharePct: 10.0}, // output — kept high enough that input/output (3.75x) stays under the ratio bound, isolating the ceiling check
	}}
	bad := fit.implausibleColumns(testGate)
	if len(bad) != 1 || !strings.Contains(bad[0], "input") {
		t.Fatalf("implausibleColumns = %v, want exactly one entry naming input", bad)
	}
	if !strings.Contains(bad[0], "150.00") || !strings.Contains(bad[0], "100.00") {
		t.Errorf("implausibleColumns = %v, want the offending value and the ceiling in the message", bad)
	}
}

// cache_read priced at far less than a plausible fraction of input — the
// shape every one of the real store's bad rows had (input in the
// hundreds, cache_read a fraction of a cent) — must be rejected even when
// input itself is under the absolute ceiling.
func TestImplausibleColumns_CacheReadFarBelowInput(t *testing.T) {
	fit := fitResult{Columns: [4]fitColumn{
		{Coef: 80.0, Participates: true, SharePct: 15.0}, // input — under the 100.0 ceiling on its own
		{Coef: 0.01, Participates: true, SharePct: 75.0}, // cache_read — 1/8000th of input
		{},
		{Coef: 20.0, Participates: true, SharePct: 10.0}, // output — kept high enough that input/output (4x) stays under the ratio bound, isolating the cache_read check
	}}
	bad := fit.implausibleColumns(testGate)
	if len(bad) != 1 || !strings.Contains(bad[0], "cache_read/input") {
		t.Fatalf("implausibleColumns = %v, want exactly one cache_read/input entry", bad)
	}
}

// Input priced many times above output — upside down for every real price
// table this project checked against — must be rejected even when every
// individual column is under the absolute ceiling.
func TestImplausibleColumns_InputExceedsOutputRatio(t *testing.T) {
	fit := fitResult{Columns: [4]fitColumn{
		{Coef: 90.0, Participates: true, SharePct: 20.0},
		{Coef: 0.15, Participates: true, SharePct: 65.0},
		{},
		{Coef: 6.0, Participates: true, SharePct: 15.0}, // input/output = 15x, over the 5.0 ceiling
	}}
	bad := fit.implausibleColumns(testGate)
	if len(bad) != 1 || !strings.Contains(bad[0], "input/output") {
		t.Fatalf("implausibleColumns = %v, want exactly one input/output entry", bad)
	}
}

// A model that simply doesn't price cache reads at all (Coef 0, common in
// real catalogs) must not trip the cache_read/input consistency check —
// there's nothing inconsistent about a model that doesn't discount cache
// hits, only about one that claims to and prices it absurdly low.
func TestImplausibleColumns_ZeroCacheReadNotFlagged(t *testing.T) {
	fit := fitResult{Columns: [4]fitColumn{
		{Coef: 5.0, Participates: true, SharePct: 25.0},
		{Coef: 0, Participates: false, SharePct: 0},
		{},
		{Coef: 25.0, Participates: true, SharePct: 75.0},
	}}
	if bad := fit.implausibleColumns(testGate); len(bad) != 0 {
		t.Errorf("implausibleColumns = %v, want none — a model with no cache-read pricing at all isn't inconsistent", bad)
	}
}

// A well-conditioned, realistically-priced fit (Claude's real published
// rates, well within every bound) must not be flagged — this gate isn't a
// blanket penalty on every fit, same as the identifiability gate isn't
// (see TestFitModelRates_WellConditionedModelNotFlagged).
func TestImplausibleColumns_RealisticRatesNotFlagged(t *testing.T) {
	fit := fitResult{Columns: [4]fitColumn{
		{Coef: 5.0, Participates: true, SharePct: 20.0},
		{Coef: 0.5, Participates: true, SharePct: 20.0},
		{Coef: 6.25, Participates: true, SharePct: 20.0},
		{Coef: 25.0, Participates: true, SharePct: 40.0},
	}}
	if bad := fit.implausibleColumns(testGate); len(bad) != 0 {
		t.Errorf("implausibleColumns = %v, want none for realistic, well-proportioned rates", bad)
	}
}

// maxObservedCatalogRate must read the real ceiling out of whatever
// models.dev rows are in the store, across every column and provider, not
// just the provider a derive run happens to be pricing.
func TestMaxObservedCatalogRate_ReadsAcrossProvidersAndColumns(t *testing.T) {
	st := withDeriveStore(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "openai", Model: "o1-pro", Input: 15.0, Output: 60.0, Source: priceSourceModelsDev, UpdatedAt: 1},
		{Provider: "anthropic", Model: "claude-opus-5", Input: 5.0, Output: 25.0, CacheRead: 75.0, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	max, ok := maxObservedCatalogRate(context.Background(), st)
	if !ok {
		t.Fatal("expected a catalog to be found")
	}
	if max != 75.0 {
		t.Errorf("max = %v, want 75.0 (the CacheRead outlier, not just the highest Input)", max)
	}
}

// A store with no models.dev catalog synced at all must fall back rather
// than gate on a ceiling of 0, which would reject every real fit.
func TestMaxObservedCatalogRate_NoCatalogReportsMiss(t *testing.T) {
	st := withDeriveStore(t)
	if _, ok := maxObservedCatalogRate(context.Background(), st); ok {
		t.Fatal("expected no catalog to be found in a fresh store")
	}
	gate := defaultPlausibilityGate(context.Background(), st, 0)
	if gate.MaxRatePerM != plausibilityCeilingFallback {
		t.Errorf("MaxRatePerM = %v, want the hardcoded fallback %v", gate.MaxRatePerM, plausibilityCeilingFallback)
	}
}

// An explicit --max-plausible-rate override must win outright over
// whatever the catalog would otherwise compute.
func TestDefaultPlausibilityGate_ExplicitOverrideWins(t *testing.T) {
	st := withDeriveStore(t)
	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "openai", Model: "o1-pro", Input: 150.0, Output: 600.0, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	gate := defaultPlausibilityGate(context.Background(), st, 42.0)
	if gate.MaxRatePerM != 42.0 {
		t.Errorf("MaxRatePerM = %v, want the explicit override 42.0, not one computed from the catalog", gate.MaxRatePerM)
	}
}

// placeholderPattern is the shared cache_read/output token pattern behind
// the near-constant-input fixtures below — six messages' worth of
// independently-varying (cr, out) pairs, cycled to build a longer series.
type placeholderTok struct{ cr, out int }

var placeholderPattern = []placeholderTok{
	{500, 200}, {300, 800}, {1200, 150}, {100, 900},
	{700, 300}, {2000, 50},
}

// The real-world case the variance pre-filter exists for: a model whose
// input_tokens is a near-constant tiny placeholder (opencode logs
// input=3 for the overwhelming majority of some models' messages, GPT 5.6
// Sol's is exactly 3 in all 37 of its real messages) has nothing in the
// data to pin down an input rate from — the column is mathematically
// arbitrary, not merely imprecise. Before fitModelRates gained the
// pre-filter, this let NNLS assign input an implausible coefficient
// anyway (this is literally why plausibilityGate exists — see the real
// $602/M, $364/M, $332/M, $255/M rows this project found and purged).
//
// This test used to assert the opposite — that the segment gets
// rejected as implausible — because at the time fitModelRates had no way
// to tell "unmeasurable" apart from "measured, however implausible": it
// let the near-constant column fit freely and relied on the magnitude
// gate to catch the result downstream. Now that fitModelRates pins a
// near-constant column with negligible token share (see its doc comment),
// the segment is no longer implausible at all — input never gets a
// chance to drift, so there's nothing for the magnitude gate to catch,
// and the segment is stored with input correctly reported as pinned
// rather than fitted.
func TestDeriveModelRates_PinsNearConstantPlaceholderWithNegligibleShare(t *testing.T) {
	st := withDeriveStore(t)

	const trueCR, trueOut = 0.11, 6.6
	var msgs []testStep
	for i := 0; i < 100; i++ {
		tk := placeholderPattern[i%len(placeholderPattern)]
		// Noise-free: input contributes nothing to the true cost (as it
		// should, since the token count is a placeholder that carries no
		// real information), so cache_read/output's recovered rates can be
		// checked against an exact answer, not just "didn't crash".
		cost := float64(tk.cr)*trueCR/compute.TokensPerMillion + float64(tk.out)*trueOut/compute.TokensPerMillion
		s := step("placeholder-model", 3, tk.cr, tk.out) // Input=3 for every message: the degenerate placeholder
		s.Cost = cost
		msgs = append(msgs, s)
	}
	ingestTotalTestSession(t, st, "opencode", "s1", msgs)

	results, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want 1 entry", results)
	}
	r := results[0]

	if r.Status != "stored" {
		t.Fatalf("status = %q, want stored (input coef=%+v, residual=%.4f%%)", r.Status, r.Fit.Columns[0], r.Fit.ResidualPct)
	}

	inCol, crCol, outCol := r.Fit.Columns[0], r.Fit.Columns[1], r.Fit.Columns[3]
	if !inCol.Pinned {
		t.Errorf("input.Pinned = false, want true — its token values are constant and carry negligible token share")
	}
	if inCol.Coef != 0 {
		t.Errorf("input.Coef = %v, want exactly 0 for a pinned column", inCol.Coef)
	}
	if !inCol.Participates {
		t.Error("input.Participates = false, want true — the column DID appear in the data (value 3), it just wasn't fit")
	}
	// The other two columns must recover their exact true rates: pinning
	// input to 0 must not distort them, since input's real contribution
	// to the noise-free cost above was already 0.
	if !approxEqual(crCol.Coef, trueCR, 1e-9) {
		t.Errorf("cache_read = %v, want %v (unpinned columns must fit correctly around a pinned one)", crCol.Coef, trueCR)
	}
	if !approxEqual(outCol.Coef, trueOut, 1e-9) {
		t.Errorf("output = %v, want %v (unpinned columns must fit correctly around a pinned one)", outCol.Coef, trueOut)
	}

	prices, err := st.GetModelPrices(context.Background(), derivedProvider("opencode"))
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("stored rows = %+v, want 1", prices)
	}
	if prices[0].Input != 0 {
		t.Errorf("stored Input = %v, want 0 (pinned)", prices[0].Input)
	}
	if !approxEqual(prices[0].CacheRead, trueCR, 1e-9) || !approxEqual(prices[0].Output, trueOut, 1e-9) {
		t.Errorf("stored rates = %+v, want CacheRead=%v Output=%v", prices[0], trueCR, trueOut)
	}
}

// A near-constant column with MATERIAL token share is a different answer:
// pinning it to 0 would silently absorb a real, non-negligible chunk of
// the bill into the surviving columns and distort them, so it must be
// rejected as unidentified instead of pinned — a constant-but-large column
// is still unidentifiable, it just can't be papered over the way a
// negligible one safely can.
func TestDeriveModelRates_RejectsMaterialConstantColumn(t *testing.T) {
	st := withDeriveStore(t)

	const constInput = 5000 // large relative to cr/out below, unlike the 3-token placeholder case
	var msgs []testStep
	for i := 0; i < 100; i++ {
		tk := placeholderPattern[i%len(placeholderPattern)]
		cost := float64(constInput)*0.3/compute.TokensPerMillion +
			float64(tk.cr)*0.11/compute.TokensPerMillion + float64(tk.out)*6.6/compute.TokensPerMillion
		s := step("big-constant-model", constInput, tk.cr, tk.out)
		s.Cost = cost
		msgs = append(msgs, s)
	}
	ingestTotalTestSession(t, st, "opencode", "s1", msgs)

	results, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want 1 entry", results)
	}
	r := results[0]

	if !strings.HasPrefix(r.Status, "rejected: unidentified") {
		t.Fatalf("status = %q, want rejected: unidentified(input) — a constant column with material token share must not be pinned", r.Status)
	}
	if r.Fit.Columns[0].Pinned {
		t.Error("input.Pinned = true, want false — material token share must not be silently assumed 0")
	}
	if len(r.Fit.ConstantUnidentified) != 1 || r.Fit.ConstantUnidentified[0] != "input" {
		t.Errorf("ConstantUnidentified = %v, want [input]", r.Fit.ConstantUnidentified)
	}

	prices, err := st.GetModelPrices(context.Background(), derivedProvider("opencode"))
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("a rejected segment must not be stored, even partially: %+v", prices)
	}
}

// A column with real variance must be completely unaffected by the
// pre-filter — this is the GLM 5.2 case in the real store (input varies
// message to message, e.g. 144, 97, 99, 172, ...) and the main regression
// risk of adding a variance-based check at all: it must never fire on data
// that genuinely lets a rate be measured.
func TestFitModelRates_RealVarianceColumnNotPinned(t *testing.T) {
	trueRates := compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4}
	var msgs []deriveMsg
	for i := 0; i < 100; i++ {
		tk := placeholderPattern[i%len(placeholderPattern)]
		in := 1000 + i*37 // varies every message, unlike the constant-placeholder fixtures above
		cost := float64(in)*trueRates.Input/compute.TokensPerMillion +
			float64(tk.cr)*trueRates.CacheRead/compute.TokensPerMillion + float64(tk.out)*trueRates.Output/compute.TokensPerMillion
		msgs = append(msgs, deriveMsg{Input: in, CacheRead: tk.cr, Output: tk.out, Cost: cost, CreatedAt: int64(1000 + i)})
	}

	fit := fitModelRates(msgs, 0.1, 1.0)
	if fit.Columns[0].Pinned {
		t.Fatalf("input.Pinned = true, want false — this column has real, measurable variance")
	}
	if len(fit.ConstantUnidentified) != 0 {
		t.Errorf("ConstantUnidentified = %v, want none", fit.ConstantUnidentified)
	}
	if !approxEqual(fit.Columns[0].Coef, trueRates.Input, 1e-6) {
		t.Errorf("input = %v, want %v", fit.Columns[0].Coef, trueRates.Input)
	}
}

// deriveSeries builds n noise-free deriveMsgs at the given rates, one
// millisecond apart starting at startMs — a clean synthetic series for
// detectSegments tests, where the only thing that should ever cause a
// segment boundary is the rate itself changing.
func deriveSeries(n int, startMs int64, rates compute.ModelPrices) []deriveMsg {
	type tok struct{ in, cr, out int }
	// A small repeating, non-degenerate token-mix pattern so every column
	// actually participates and carries a real, non-arbitrary share —
	// exercising the tolerance/merge logic on a fit that's stable and
	// identified, not one detectSegments would reject before it even gets
	// to compare adjacent blocks.
	pattern := []tok{
		{5000, 500, 200}, {8000, 300, 800}, {3000, 1200, 150}, {10000, 100, 900},
		{6000, 700, 300}, {1000, 2000, 50},
	}
	msgs := make([]deriveMsg, n)
	for i := 0; i < n; i++ {
		tk := pattern[i%len(pattern)]
		cost := float64(tk.in)*rates.Input/compute.TokensPerMillion +
			float64(tk.cr)*rates.CacheRead/compute.TokensPerMillion +
			float64(tk.out)*rates.Output/compute.TokensPerMillion
		msgs[i] = deriveMsg{Input: tk.in, CacheRead: tk.cr, Output: tk.out, Cost: cost, CreatedAt: startMs + int64(i)}
	}
	return msgs
}

// A model whose rate never changed must produce exactly one segment,
// covering the whole series and fully open on both ends — the regression
// this whole feature must not break for PI's currently-exact models.
func TestDetectSegments_StableSeriesYieldsOneSegment(t *testing.T) {
	msgs := deriveSeries(100, 1000, compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4})

	segs := detectSegments(msgs, 5, 15.0, 0.1, 1.0)
	if len(segs) != 1 {
		t.Fatalf("len(segs) = %d, want 1: %+v", len(segs), segs)
	}
	if segs[0].From != 0 || segs[0].To != 0 {
		t.Errorf("From/To = %d/%d, want 0/0 (fully open)", segs[0].From, segs[0].To)
	}
	if segs[0].Msgs != 100 {
		t.Errorf("Msgs = %d, want 100", segs[0].Msgs)
	}
	if !approxEqual(segs[0].Fit.Columns[3].Coef, 4.4, 1e-6) {
		t.Errorf("output rate = %v, want 4.4 (unchanged from the input rate)", segs[0].Fit.Columns[3].Coef)
	}
}

// A single rate change at a known index must yield exactly two segments,
// split at that index, each recovering its own true rate.
//
// The boundary sits at a multiple of detectSegments' initial chunk size
// (minMessages * segmentInitialChunkFactor = 5*5 = 25) deliberately: a
// change that instead lands mid-chunk pollutes that one coarse block with a
// mix of both rates, which is a real, separately-documented limit of
// starting from fixed-size blocks (see prices_derive.go's doc comment and
// this task's report) — not what this test is checking. Locating a change
// to within one coarse block is exactly what a real run's residual gate
// already turns into an honest "rejected: error" for that narrow span
// rather than a silently wrong rate; this test asserts the clean case
// where the change point is caught exactly.
func TestDetectSegments_OneChangeYieldsTwoSegmentsAtTheBoundary(t *testing.T) {
	const boundaryIdx = 75 // a multiple of chunkSize (minMessages=5 * factor=5 = 25)
	before := deriveSeries(boundaryIdx, 1000, compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4})
	after := deriveSeries(75, 1000+int64(boundaryIdx), compute.ModelPrices{Input: 0.2, CacheRead: 0.04, Output: 0.6})
	msgs := append(before, after...)

	segs := detectSegments(msgs, 5, 15.0, 0.1, 1.0)
	if len(segs) != 2 {
		t.Fatalf("len(segs) = %d, want 2: %+v", len(segs), segs)
	}

	wantBoundary := msgs[boundaryIdx].CreatedAt
	if segs[0].From != 0 {
		t.Errorf("segs[0].From = %d, want 0 (open at the start)", segs[0].From)
	}
	if segs[0].To != wantBoundary {
		t.Errorf("segs[0].To = %d, want %d (the first post-change message's timestamp)", segs[0].To, wantBoundary)
	}
	if segs[1].From != wantBoundary {
		t.Errorf("segs[1].From = %d, want %d", segs[1].From, wantBoundary)
	}
	if segs[1].To != 0 {
		t.Errorf("segs[1].To = %d, want 0 (open at the end)", segs[1].To)
	}
	if segs[0].Msgs != boundaryIdx || segs[1].Msgs != 75 {
		t.Errorf("Msgs = %d/%d, want %d/%d", segs[0].Msgs, segs[1].Msgs, boundaryIdx, 75)
	}
	if !approxEqual(segs[0].Fit.Columns[3].Coef, 4.4, 1e-6) {
		t.Errorf("segs[0] output rate = %v, want 4.4 (the pre-change rate)", segs[0].Fit.Columns[3].Coef)
	}
	if !approxEqual(segs[1].Fit.Columns[3].Coef, 0.6, 1e-6) {
		t.Errorf("segs[1] output rate = %v, want 0.6 (the post-change rate)", segs[1].Fit.Columns[3].Coef)
	}
}

// A single message with an oddball cost — unrelated to any real rate
// change — must not split the series: it's outvoted by the dozens of
// ordinary messages sharing its block (see detectSegments' doc comment).
//
// minMessages=20 here (not the 5 the tests above use) so the block one bad
// message lands in has the same 100-message scale a real derive run's
// default does — the dilution that makes one outlier harmless is a
// function of how many ordinary messages share its block, so a test using
// a tiny block would only prove the claim at a scale nothing actually runs
// at.
func TestDetectSegments_SingleOutlierDoesNotCreateASegment(t *testing.T) {
	msgs := deriveSeries(200, 1000, compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4})
	// One message, in the first 100-message block, billed at 1.5x — a real
	// anomaly (a retry, a burst-priced request), but a single one. Thin
	// (low cost-share) columns are the most outlier-sensitive in a small
	// block — a single message can still shift one enough that a much
	// larger multiplier here would legitimately fail to merge, which would
	// be testing the wrong thing: the claim under test is "one odd message
	// doesn't split the series", not "no perturbation ever moves a fit".
	msgs[30].Cost *= 1.5

	segs := detectSegments(msgs, 20, 15.0, 0.1, 1.0)
	if len(segs) != 1 {
		t.Fatalf("len(segs) = %d, want 1 (one outlier message must not split the series): %+v", len(segs), segs)
	}
	if segs[0].Msgs != 200 {
		t.Errorf("Msgs = %d, want 200", segs[0].Msgs)
	}
}

// Two genuine rate changes must yield three segments, each recovering its
// own rate — detectSegments isn't hardcoded to "at most one change". Every
// boundary sits at a multiple of chunkSize (25), for the same reason
// TestDetectSegments_OneChangeYieldsTwoSegmentsAtTheBoundary's does.
func TestDetectSegments_TwoChangesYieldThreeSegments(t *testing.T) {
	seg1 := deriveSeries(75, 1000, compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4})
	seg2 := deriveSeries(75, 1075, compute.ModelPrices{Input: 0.2, CacheRead: 0.04, Output: 0.6})
	seg3 := deriveSeries(75, 1150, compute.ModelPrices{Input: 3.0, CacheRead: 0.5, Output: 10.0})
	msgs := append(append(seg1, seg2...), seg3...)

	segs := detectSegments(msgs, 5, 15.0, 0.1, 1.0)
	if len(segs) != 3 {
		t.Fatalf("len(segs) = %d, want 3: %+v", len(segs), segs)
	}
	if !approxEqual(segs[0].Fit.Columns[3].Coef, 4.4, 1e-6) ||
		!approxEqual(segs[1].Fit.Columns[3].Coef, 0.6, 1e-6) ||
		!approxEqual(segs[2].Fit.Columns[3].Coef, 10.0, 1e-6) {
		t.Errorf("output rates = %v/%v/%v, want 4.4/0.6/10.0",
			segs[0].Fit.Columns[3].Coef, segs[1].Fit.Columns[3].Coef, segs[2].Fit.Columns[3].Coef)
	}
}

// A segment that fails the residual gate must be rejected without
// affecting its siblings' fits or storage — deriveModelRates gates each
// segment independently, exactly as it gated the single whole-window fit
// before segmentation existed.
func TestDeriveModelRates_OneBadSegmentDoesNotAffectSiblings(t *testing.T) {
	st := withDeriveStore(t)

	// gatherDeriveMessages filters by created_at >= now - days*24h, so this
	// test's messages need real, recent timestamps — unlike the pure
	// detectSegments tests above, this one goes through the full
	// deriveModelRates path with a days window.
	startMs := time.Now().Add(-10 * 24 * time.Hour).UnixMilli()

	// Every segment is 75 messages — a multiple of chunkSize (minMessages=5
	// * segmentInitialChunkFactor=5 = 25) — so the boundaries between good
	// and bad data land exactly on a coarse-block edge instead of
	// polluting one block with a mix of good and bad rows; see
	// TestDetectSegments_OneChangeYieldsTwoSegmentsAtTheBoundary's comment.
	goodRates := compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4}
	good1 := deriveSeries(75, startMs, goodRates)
	// A middle segment with costs unrelated to any linear rate table —
	// same shape as TestFitModelRates_BadDataHasLargeResidual, but placed
	// between two good segments instead of standing alone.
	var bad []deriveMsg
	for i, m := range deriveSeries(75, startMs+75, goodRates) {
		m.Cost = 0.01
		if i%2 == 0 {
			m.Cost = 50.0
		}
		bad = append(bad, m)
	}
	good2 := deriveSeries(75, startMs+150, goodRates)

	msgs := append(append(good1, bad...), good2...)
	steps := make([]testStep, len(msgs))
	for i, m := range msgs {
		s := step("sandwiched-model", m.Input, m.CacheRead, m.Output)
		s.Cost = m.Cost
		steps[i] = s
	}
	// ingestTotalTestSession stamps every message with the same CreatedAt
	// (see helpers_test.go), which would collapse the very time ordering
	// this test depends on — so this test seeds the store directly instead,
	// preserving each message's own timestamp.
	seedDeriveMessages(t, st, "opencode", "sandwiched-model", msgs)

	results, err := deriveModelRates(context.Background(), st, 30, 5, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v, want 3 segments", results)
	}
	if results[0].Status != "stored" {
		t.Errorf("first segment status = %q, want stored (residual %.2f%%)", results[0].Status, results[0].Fit.ResidualPct)
	}
	if results[1].Status != "rejected: error" {
		t.Errorf("middle segment status = %q, want rejected: error", results[1].Status)
	}
	if results[2].Status != "stored" {
		t.Errorf("last segment status = %q, want stored (residual %.2f%%)", results[2].Status, results[2].Fit.ResidualPct)
	}

	prices, err := st.GetModelPrices(context.Background(), derivedProvider("opencode"))
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("stored windows = %+v, want 2 (the bad middle segment must not be written)", prices)
	}
	for _, p := range prices {
		if !approxEqual(p.Output, goodRates.Output, 1e-6) {
			t.Errorf("stored window %+v has the wrong rate — a bad sibling must not have perturbed it", p)
		}
	}
}

// seedDeriveMessages writes deriveMsgs straight into the store, preserving
// each one's own CreatedAt — ingestTotalTestSession stamps every message
// in a session with the same timestamp, which is fine for tests that don't
// care about ordering but wrong for anything exercising detectSegments.
func seedDeriveMessages(t *testing.T, st *store.Store, agent, model string, msgs []deriveMsg) {
	t.Helper()
	parsed := make([]store.ParsedMessage, len(msgs))
	for i, m := range msgs {
		parsed[i] = store.ParsedMessage{Message: store.Message{
			Agent: agent, SessionID: "seeded-session", MsgIndex: i, Role: store.RoleAssistant,
			Model: model, InputTokens: m.Input, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite,
			OutputTokens: m.Output, Cost: m.Cost, CreatedAt: m.CreatedAt,
		}}
	}
	ps := store.ParsedSession{
		Session:  store.Session{Agent: agent, SessionID: "seeded-session", CreatedAt: msgs[0].CreatedAt, LastActivity: msgs[len(msgs)-1].CreatedAt},
		Messages: parsed,
	}
	if err := st.IngestSession(context.Background(), ps); err != nil {
		t.Fatalf("IngestSession: %v", err)
	}
}

// withDeriveStore opens a fresh temp store the same way withTempStore does
// (see prices_test.go), but hands the *store.Store back directly since
// deriveModelRates takes it as a parameter rather than reading the global.
func withDeriveStore(t *testing.T) *store.Store {
	t.Helper()
	return withTempStore(t)
}

// goodModelSteps builds messages for "good-model" whose cost is exactly
// linear in (input, cache_read, output) at the given rates — the shared
// well-conditioned fixture used by several tests below.
func goodModelSteps(name string, rates compute.ModelPrices) []testStep {
	type tok struct{ in, cr, out int }
	toks := []tok{
		{5000, 500, 200}, {8000, 300, 800}, {3000, 1200, 150}, {10000, 100, 900},
		{6000, 700, 300}, {1000, 2000, 50}, {12000, 50, 1200}, {4000, 900, 400},
		{7000, 250, 600}, {2000, 1500, 100}, {9000, 400, 700}, {1500, 1800, 80},
		{6500, 600, 250}, {3500, 1100, 180}, {8800, 220, 760}, {2600, 1700, 130},
		{9900, 90, 850}, {4400, 980, 420}, {7700, 310, 640}, {1800, 1600, 95},
		{5600, 470, 260}, {9200, 130, 810},
	}
	var out []testStep
	for _, tk := range toks {
		cost := compute.PiStepActualCost(compute.StepData{Input: tk.in, CacheRead: tk.cr, Output: tk.out}, rates)
		s := step(name, tk.in, tk.cr, tk.out)
		s.Cost = cost
		out = append(out, s)
	}
	return out
}

// The store-level path: ingest messages with a known, exactly-recoverable
// rate for one pi model, and a second pi model whose logged costs don't fit
// any linear rate table. deriveModelRates must store the first (source =
// "derived", provider "derived:pi", numbers matching the true rate) and
// reject the second without writing anything for it.
func TestDeriveModelRates_StoresGoodFitRejectsBad(t *testing.T) {
	st := withDeriveStore(t)

	trueRates := compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4}
	ingestTotalTestSession(t, st, "pi", "good-session", goodModelSteps("good-model", trueRates))

	// A model with enough messages (so it isn't rejected for sample size)
	// but costs that bear no linear relationship to its tokens.
	type tok struct{ in, cr, out int }
	toks := []tok{
		{5000, 500, 200}, {8000, 300, 800}, {3000, 1200, 150}, {10000, 100, 900},
		{6000, 700, 300}, {1000, 2000, 50}, {12000, 50, 1200}, {4000, 900, 400},
		{7000, 250, 600}, {2000, 1500, 100}, {9000, 400, 700}, {1500, 1800, 80},
		{6500, 600, 250}, {3500, 1100, 180}, {8800, 220, 760}, {2600, 1700, 130},
		{9900, 90, 850}, {4400, 980, 420}, {7700, 310, 640}, {1800, 1600, 95},
		{5600, 470, 260}, {9200, 130, 810},
	}
	var bad []testStep
	for i, tk := range toks {
		s := step("bad-model", tk.in, tk.cr, tk.out)
		if i%2 == 0 {
			s.Cost = 0.01
		} else {
			s.Cost = 50.0
		}
		bad = append(bad, s)
	}
	ingestTotalTestSession(t, st, "pi", "bad-session", bad)

	results, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}

	var good_, bad_ *deriveResult
	for i := range results {
		switch results[i].Model {
		case "good-model":
			good_ = &results[i]
		case "bad-model":
			bad_ = &results[i]
		}
	}
	if good_ == nil || bad_ == nil {
		t.Fatalf("expected results for both models, got %+v", results)
	}

	if good_.Status != "stored" {
		t.Errorf("good-model status = %q, want %q (residual %.4f%%)", good_.Status, "stored", good_.Fit.ResidualPct)
	}
	if bad_.Status != "rejected: error" {
		t.Errorf("bad-model status = %q, want %q", bad_.Status, "rejected: error")
	}

	prices, err := st.GetModelPrices(context.Background(), derivedProvider("pi"))
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	var goodRow *store.ModelPrice
	for i := range prices {
		if prices[i].Model == "good-model" {
			goodRow = &prices[i]
		}
		if prices[i].Model == "bad-model" {
			t.Errorf("bad-model must not be stored, found row: %+v", prices[i])
		}
	}
	if goodRow == nil {
		t.Fatalf("good-model not found in stored prices: %+v", prices)
	}
	if goodRow.Provider != "derived:pi" {
		t.Errorf("Provider = %q, want %q", goodRow.Provider, "derived:pi")
	}
	if goodRow.Source != priceSourceDerived {
		t.Errorf("Source = %q, want %q", goodRow.Source, priceSourceDerived)
	}
	if !approxEqual(goodRow.Input, trueRates.Input, 1e-6) {
		t.Errorf("Input = %v, want %v", goodRow.Input, trueRates.Input)
	}
	if !approxEqual(goodRow.CacheRead, trueRates.CacheRead, 1e-6) {
		t.Errorf("CacheRead = %v, want %v", goodRow.CacheRead, trueRates.CacheRead)
	}
	if !approxEqual(goodRow.Output, trueRates.Output, 1e-6) {
		t.Errorf("Output = %v, want %v", goodRow.Output, trueRates.Output)
	}
	if goodRow.CacheWrite != 0 {
		t.Errorf("CacheWrite = %v, want 0 (degenerate column — no cache-write usage in the fixture)", goodRow.CacheWrite)
	}
	if goodRow.CacheWrite1h != 0 {
		t.Errorf("CacheWrite1h = %v, want 0 (Claude-only field, must not be derived here)", goodRow.CacheWrite1h)
	}
}

// A model that passes the residual gate but has a column with a negligible
// cost share must be rejected as unidentified, and nothing for it written
// to the store — a half-derived row (some columns real, one arbitrary)
// would look complete and isn't.
func TestDeriveModelRates_RejectsUnidentifiedColumn(t *testing.T) {
	st := withDeriveStore(t)

	trueRates := []float64{0.0005, 0.5, 0.0, 5.0}
	type tok struct{ in, cr, out int }
	toks := []tok{
		{5000, 500, 200}, {8000, 300, 800}, {3000, 1200, 150}, {10000, 100, 900},
		{6000, 700, 300}, {1000, 2000, 50}, {12000, 50, 1200}, {4000, 900, 400},
		{7000, 250, 600}, {2000, 1500, 100}, {9000, 400, 700}, {1500, 1800, 80},
		{6500, 600, 250}, {3500, 1100, 180}, {8800, 220, 760}, {2600, 1700, 130},
		{9900, 90, 850}, {4400, 980, 420}, {7700, 310, 640}, {1800, 1600, 95},
		{5600, 470, 260}, {9200, 130, 810},
	}
	var msgs []testStep
	for _, tk := range toks {
		cost := float64(tk.in)*trueRates[0]/compute.TokensPerMillion +
			float64(tk.cr)*trueRates[1]/compute.TokensPerMillion +
			float64(tk.out)*trueRates[3]/compute.TokensPerMillion
		s := step("low-share-model", tk.in, tk.cr, tk.out)
		s.Cost = cost
		msgs = append(msgs, s)
	}
	ingestTotalTestSession(t, st, "opencode", "s1", msgs)

	results, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want 1 entry", results)
	}
	if results[0].Status != "rejected: unidentified(input)" {
		t.Errorf("status = %q, want %q (residual %.4f%%, input share %.4f%%)",
			results[0].Status, "rejected: unidentified(input)", results[0].Fit.ResidualPct, results[0].Fit.Columns[0].SharePct)
	}

	prices, err := st.GetModelPrices(context.Background(), derivedProvider("opencode"))
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("an unidentified fit must not be stored, even partially: %+v", prices)
	}
}

// --dry-run must fit and report exactly as a real run would, but write
// nothing to the store.
func TestDeriveModelRates_DryRunWritesNothing(t *testing.T) {
	st := withDeriveStore(t)

	trueRates := compute.ModelPrices{Input: 0.95, CacheRead: 0.16, Output: 4.0}
	ingestTotalTestSession(t, st, "pi", "s1", goodModelSteps("dry-run-model", trueRates)[:20])

	results, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, true)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 1 || results[0].Status != "dry-run" {
		t.Fatalf("results = %+v, want one dry-run entry", results)
	}

	prices, err := st.GetModelPrices(context.Background(), "")
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("dry-run must not write to the store, found: %+v", prices)
	}
}

// Sample size below --min-messages must be reported and skipped without an
// attempted fit — not silently dropped, and not fitted on too little data.
func TestDeriveModelRates_SkipsSmallSample(t *testing.T) {
	st := withDeriveStore(t)

	ingestTotalTestSession(t, st, "opencode", "s1", []testStep{
		loggedStep("tiny-model", 1000, 100, 50, 0.5),
		loggedStep("tiny-model", 2000, 200, 100, 1.0),
	})

	results, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want 1 entry", results)
	}
	if results[0].Status != "rejected: sample size" {
		t.Errorf("status = %q, want %q", results[0].Status, "rejected: sample size")
	}
	if results[0].Msgs != 2 {
		t.Errorf("Msgs = %d, want 2", results[0].Msgs)
	}
}

// The claude agent must never be considered for derivation: its
// message.cost is tokeneks' own recomputation, not a provider-billed
// figure, so fitting against it would be circular.
func TestDeriveModelRates_ExcludesClaude(t *testing.T) {
	st := withDeriveStore(t)

	var msgs []testStep
	for i := 0; i < 30; i++ {
		msgs = append(msgs, loggedStep("claude-sonnet-5", 10000, 0, 2000, 999.99))
	}
	ingestTotalTestSession(t, st, "claude", "s1", msgs)

	results, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none — claude must be excluded entirely", results)
	}
}

// A row left over from the old bare-agent-name provider scheme ("pi",
// "opencode") shares a PK namespace with models.dev's own real "opencode"
// provider (see derivedProvider) and must be purged unconditionally, even
// when the run that purges it has nothing else to derive.
func TestDeriveModelRates_PurgesOldSchemeProviderRows(t *testing.T) {
	st := withDeriveStore(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{
		{Provider: "pi", Model: "stale-old-scheme", Input: 1, Output: 1, Source: priceSourceDerived, UpdatedAt: 1},
		{Provider: "opencode", Model: "stale-old-scheme", Input: 1, Output: 1, Source: priceSourceDerived, UpdatedAt: 1},
		// A real models.dev row under provider "opencode" must survive —
		// the purge is scoped to source="derived", not to the provider alone.
		{Provider: "opencode", Model: "claude-opus-4-8", Input: 5, Output: 25, Source: priceSourceModelsDev, UpdatedAt: 1},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false); err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}

	prices, err := st.GetModelPrices(context.Background(), "")
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	for _, p := range prices {
		if p.Provider == "pi" || (p.Provider == "opencode" && p.Source == priceSourceDerived) {
			t.Errorf("old-scheme derived row survived purge: %+v", p)
		}
	}
	found := false
	for _, p := range prices {
		if p.Provider == "opencode" && p.Model == "claude-opus-4-8" && p.Source == priceSourceModelsDev {
			found = true
		}
	}
	if !found {
		t.Error("a real models.dev row under provider \"opencode\" was deleted; the purge must be scoped to source=derived")
	}
}

// Re-running derive after a model's fit no longer qualifies (gate tightened,
// or the underlying data changed) must retract the previously stored rate,
// not leave a stale number looking current.
func TestDeriveModelRates_DeletesStaleRowWhenNoLongerQualifies(t *testing.T) {
	st := withDeriveStore(t)

	trueRates := compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4}
	ingestTotalTestSession(t, st, "pi", "s1", goodModelSteps("flaky-model", trueRates))

	if _, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 1.0, 0.1, 15.0, 0, false); err != nil {
		t.Fatalf("first deriveModelRates: %v", err)
	}
	prices, err := st.GetModelPrices(context.Background(), derivedProvider("pi"))
	if err != nil || len(prices) != 1 {
		t.Fatalf("expected flaky-model stored after first run, got %+v (err=%v)", prices, err)
	}

	// Tighten --max-error past what this fixture's residual (~0%, but not
	// exactly 0 due to floating point) can ever fail — instead simulate a
	// gate getting stricter by tightening --min-column-share past this
	// fixture's real (comfortably large) shares, which the fixture cannot
	// satisfy, forcing rejection on the second run.
	if _, err := deriveModelRates(context.Background(), st, 30, 20, 1.0, 100.0, 0.1, 15.0, 0, false); err != nil {
		t.Fatalf("second deriveModelRates: %v", err)
	}
	prices, err = st.GetModelPrices(context.Background(), derivedProvider("pi"))
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 0 {
		t.Errorf("flaky-model should have been retracted once it no longer qualifies, found: %+v", prices)
	}
}

// A rejected segment sitting between two surviving ones must not leave a
// gap: the earlier survivor's To is left alone and the later survivor's
// From is pulled backward to meet it, exactly as closeSegmentGaps' doc
// comment describes. The rejected segment's own window is untouched — the
// report still shows where the rejected span actually was.
func TestCloseSegmentGaps_FillsRejectedMiddleGap(t *testing.T) {
	results := []deriveResult{
		{From: 0, To: 100, Status: "stored"},
		{From: 100, To: 200, Status: "rejected: error"},
		{From: 200, To: 0, Status: "stored"},
	}
	closeSegmentGaps(results)

	if results[0].From != 0 || results[0].To != 100 {
		t.Errorf("results[0] = %+v, want From=0 To=100 (unchanged — it already reaches back to 0, and its own To is where the gap starts)", results[0])
	}
	if results[2].From != 100 {
		t.Errorf("results[2].From = %d, want 100 (pulled backward from 200 to meet results[0].To, absorbing the rejected gap)", results[2].From)
	}
	if results[2].To != 0 {
		t.Errorf("results[2].To = %d, want 0 (open)", results[2].To)
	}
	if results[1].From != 100 || results[1].To != 200 {
		t.Errorf("results[1] = %+v, want unchanged From=100 To=200 — a rejected segment's own window is never rewritten", results[1])
	}
}

// The earliest surviving window must reach back to 0 even when it wasn't
// detectSegments' own first segment — i.e. when the true first segment got
// rejected. Without this, a model whose oldest messages happened to land in
// a rejected block would have no price at all for "since forever" up to
// wherever the first surviving segment starts.
func TestCloseSegmentGaps_RejectedFirstSegmentReachesBackToZero(t *testing.T) {
	results := []deriveResult{
		{From: 0, To: 100, Status: "rejected: error"},
		{From: 100, To: 0, Status: "stored"},
	}
	closeSegmentGaps(results)

	if results[1].From != 0 {
		t.Errorf("results[1].From = %d, want 0 — the only surviving window must reach back to the start of time", results[1].From)
	}
	if results[1].To != 0 {
		t.Errorf("results[1].To = %d, want 0", results[1].To)
	}
	if results[0].From != 0 || results[0].To != 100 {
		t.Errorf("results[0] = %+v, want unchanged — a rejected segment's own window is never rewritten", results[0])
	}
}

// The latest surviving window must stay open (To=0) even when it wasn't
// detectSegments' own last segment — i.e. when the true last segment got
// rejected. Without this, a model's most recent traffic (everything after
// the rejected trailing segment started) would resolve to no price at all.
func TestCloseSegmentGaps_RejectedLastSegmentStaysOpen(t *testing.T) {
	results := []deriveResult{
		{From: 0, To: 100, Status: "stored"},
		{From: 100, To: 0, Status: "rejected: error"},
	}
	closeSegmentGaps(results)

	if results[0].From != 0 {
		t.Errorf("results[0].From = %d, want 0", results[0].From)
	}
	if results[0].To != 0 {
		t.Errorf("results[0].To = %d, want 0 — forced open now that it's the last surviving window", results[0].To)
	}
	if results[1].From != 100 || results[1].To != 0 {
		t.Errorf("results[1] = %+v, want unchanged — a rejected segment's own window is never rewritten", results[1])
	}
}

// A timeline with no rejected segments at all — every block already tiles,
// as detectSegments guarantees on its own — must come out of
// closeSegmentGaps byte-for-byte identical. The stitching logic must be a
// no-op on the common case, not just correct on the gap case.
func TestCloseSegmentGaps_AlreadyContiguousUnchanged(t *testing.T) {
	results := []deriveResult{
		{From: 0, To: 100, Status: "stored"},
		{From: 100, To: 200, Status: "stored"},
		{From: 200, To: 0, Status: "stored"},
	}
	want := append([]deriveResult(nil), results...)
	closeSegmentGaps(results)
	for i := range results {
		if results[i].From != want[i].From || results[i].To != want[i].To {
			t.Errorf("results[%d] = %+v, want unchanged %+v", i, results[i], want[i])
		}
	}
}

// A (agent, model) with no surviving segment at all — every one of
// detectSegments' blocks rejected — must be left completely untouched:
// there is nothing to stitch, and nothing must be invented.
func TestCloseSegmentGaps_NoSurvivorsIsNoop(t *testing.T) {
	results := []deriveResult{
		{From: 0, To: 100, Status: "rejected: error"},
		{From: 100, To: 0, Status: "rejected: implausible(input)"},
	}
	want := append([]deriveResult(nil), results...)
	closeSegmentGaps(results)
	for i := range results {
		if results[i].From != want[i].From || results[i].To != want[i].To {
			t.Errorf("results[%d] = %+v, want unchanged %+v", i, results[i], want[i])
		}
	}
}

// --dry-run's preview must show exactly the same stitched windows a real
// run would store — closeSegmentGaps must treat "dry-run" as a surviving
// status identically to "stored", or the dry-run preview a user inspects
// before committing (per this task's own verification instructions) would
// misrepresent what the real run is about to write.
func TestCloseSegmentGaps_TreatsDryRunAsSurviving(t *testing.T) {
	results := []deriveResult{
		{From: 0, To: 100, Status: "dry-run"},
		{From: 100, To: 200, Status: "rejected: error"},
		{From: 200, To: 0, Status: "dry-run"},
	}
	closeSegmentGaps(results)

	if results[2].From != 100 {
		t.Errorf("results[2].From = %d, want 100 — dry-run rows must be stitched exactly like stored ones", results[2].From)
	}
}

// The store-level regression this whole feature exists to fix: a rejected
// segment sandwiched between two good ones (same fixture as
// TestDeriveModelRates_OneBadSegmentDoesNotAffectSiblings, which checks the
// surviving rates are unaffected) must not leave the (agent, model)
// timeline with an uncovered instant. Before closeSegmentGaps existed, the
// two stored windows here kept detectSegments' original boundaries — the
// first window's To at the start of the rejected span, the second window's
// From at its end — leaving every timestamp inside the rejected span
// resolving to "no price", exactly the live-store bug this task reports.
func TestDeriveModelRates_RejectedMiddleSegmentLeavesNoGap(t *testing.T) {
	st := withDeriveStore(t)

	startMs := time.Now().Add(-10 * 24 * time.Hour).UnixMilli()
	goodRates := compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4}
	good1 := deriveSeries(75, startMs, goodRates)
	var bad []deriveMsg
	for i, m := range deriveSeries(75, startMs+75, goodRates) {
		m.Cost = 0.01
		if i%2 == 0 {
			m.Cost = 50.0
		}
		bad = append(bad, m)
	}
	good2 := deriveSeries(75, startMs+150, goodRates)
	msgs := append(append(good1, bad...), good2...)
	seedDeriveMessages(t, st, "opencode", "sandwiched-model", msgs)

	results, err := deriveModelRates(context.Background(), st, 30, 5, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v, want 3 segments", results)
	}
	if results[1].Status != "rejected: error" {
		t.Fatalf("middle segment status = %q, want rejected: error", results[1].Status)
	}

	if results[0].From != 0 {
		t.Errorf("results[0].From = %d, want 0", results[0].From)
	}
	if results[2].To != 0 {
		t.Errorf("results[2].To = %d, want 0", results[2].To)
	}
	if results[0].To != results[2].From {
		t.Errorf("results[0].To = %d, results[2].From = %d, want equal — no uncovered instant across the rejected segment", results[0].To, results[2].From)
	}

	prices, err := st.GetModelPrices(context.Background(), derivedProvider("opencode"))
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("stored windows = %+v, want 2", prices)
	}
	// GetModelPrices orders by effective_from ASC (see store/prices.go), so
	// prices[0] is the earlier window without needing to sort here.
	if prices[0].EffectiveFrom != 0 {
		t.Errorf("first stored window EffectiveFrom = %d, want 0", prices[0].EffectiveFrom)
	}
	if prices[1].EffectiveTo != 0 {
		t.Errorf("last stored window EffectiveTo = %d, want 0 (open)", prices[1].EffectiveTo)
	}
	if prices[0].EffectiveTo != prices[1].EffectiveFrom {
		t.Errorf("stored windows don't tile: first.EffectiveTo=%d second.EffectiveFrom=%d", prices[0].EffectiveTo, prices[1].EffectiveFrom)
	}
}

// The boundary/edge case a live-store defect could otherwise hide: when the
// chronologically *first* segment is the one that gets rejected, the sole
// surviving window must still reach back to 0 ("since forever"), not just
// to wherever detectSegments happened to start it.
func TestDeriveModelRates_RejectedFirstSegmentStoredWindowReachesBackToZero(t *testing.T) {
	st := withDeriveStore(t)

	startMs := time.Now().Add(-10 * 24 * time.Hour).UnixMilli()
	goodRates := compute.ModelPrices{Input: 1.4, CacheRead: 0.26, Output: 4.4}
	var bad []deriveMsg
	for i, m := range deriveSeries(75, startMs, goodRates) {
		m.Cost = 0.01
		if i%2 == 0 {
			m.Cost = 50.0
		}
		bad = append(bad, m)
	}
	good := deriveSeries(75, startMs+75, goodRates)
	msgs := append(bad, good...)
	seedDeriveMessages(t, st, "opencode", "leading-bad-model", msgs)

	results, err := deriveModelRates(context.Background(), st, 30, 5, 1.0, 1.0, 0.1, 15.0, 0, false)
	if err != nil {
		t.Fatalf("deriveModelRates: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2 segments", results)
	}
	if results[0].Status != "rejected: error" {
		t.Fatalf("first segment status = %q, want rejected: error", results[0].Status)
	}
	if results[1].Status != "stored" {
		t.Fatalf("second segment status = %q, want stored", results[1].Status)
	}
	if results[1].From != 0 {
		t.Errorf("results[1].From = %d, want 0 — the only surviving window must reach back to the start of time even though detectSegments didn't hand it From=0 itself", results[1].From)
	}

	prices, err := st.GetModelPrices(context.Background(), derivedProvider("opencode"))
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("stored windows = %+v, want 1", prices)
	}
	if prices[0].EffectiveFrom != 0 {
		t.Errorf("EffectiveFrom = %d, want 0", prices[0].EffectiveFrom)
	}
	if prices[0].EffectiveTo != 0 {
		t.Errorf("EffectiveTo = %d, want 0 (open)", prices[0].EffectiveTo)
	}
}
