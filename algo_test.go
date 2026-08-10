package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"tokeneks/compute"
)

// D3: ComputeIdeal and ComputeIdealClaude must agree when CacheCreation == 0
func TestComputeIdeal_EquivalentToClaudeWhenNoCacheCreation(t *testing.T) {
	steps := []compute.StepData{
		{Input: 1000, CacheRead: 0, Output: 200},
		{Input: 0, CacheRead: 1200, Output: 200},
		{Input: 200, CacheRead: 1000, Output: 150},
	}
	prices := compute.ModelPrices{Input: 1.0, CacheRead: 0.1, Output: 4.0}

	kimiRows := compute.ComputeIdeal(steps)
	claudeRows := compute.ComputeIdealClaude(steps, prices)

	if len(kimiRows) != len(claudeRows) {
		t.Fatalf("row count mismatch: kimi=%d claude=%d", len(kimiRows), len(claudeRows))
	}
	for i := range kimiRows {
		if kimiRows[i].IdealCR != claudeRows[i].IdealCR {
			t.Errorf("step %d: IdealCR kimi=%d claude=%d", i, kimiRows[i].IdealCR, claudeRows[i].IdealCR)
		}
		if kimiRows[i].Waste != claudeRows[i].Waste {
			t.Errorf("step %d: Waste kimi=%d claude=%d", i, kimiRows[i].Waste, claudeRows[i].Waste)
		}
		if kimiRows[i].IsCompact != claudeRows[i].IsCompact {
			t.Errorf("step %d: IsCompact kimi=%v claude=%v", i, kimiRows[i].IsCompact, claudeRows[i].IsCompact)
		}
	}
}

// C1: Summarize must not produce NaN or Inf when ideal == 0 (currently buggy — test fails until fixed)
func TestSummarize_NoDivisionByZeroOnEmptyRows(t *testing.T) {
	rows := []compute.IdealRow{}
	prices := compute.ModelPrices{Input: 1.0, CacheRead: 0.1, Output: 4.0}

	s := compute.Summarize(rows, prices)

	if math.IsNaN(s.PctIdeal) {
		t.Error("PctIdeal is NaN when ideal=0; should be 0.0")
	}
	if math.IsInf(s.PctIdeal, 0) {
		t.Error("PctIdeal is +Inf when ideal=0; should be 0.0")
	}
	if s.PctIdeal != 0.0 {
		t.Errorf("PctIdeal = %f, want 0.0 when no data", s.PctIdeal)
	}
}

// C1: SummarizeClaude must not produce NaN or Inf when ideal == 0 (currently buggy)
func TestSummarizeClaude_NoDivisionByZeroOnEmptyRows(t *testing.T) {
	rows := []compute.IdealRow{}
	prices := compute.ModelPrices{Input: 5.5, CacheCreation: 6.75, CacheRead: 0.55, Output: 27.5}

	s := compute.SummarizeClaude(rows, prices)

	if math.IsNaN(s.PctIdeal) {
		t.Error("PctIdeal is NaN when ideal=0; should be 0.0")
	}
	if math.IsInf(s.PctIdeal, 0) {
		t.Error("PctIdeal is +Inf when ideal=0; should be 0.0")
	}
	if s.PctIdeal != 0.0 {
		t.Errorf("PctIdeal = %f, want 0.0 when no data", s.PctIdeal)
	}
}

// D4: Summarize and SummarizeClaude must agree when CacheCreation == 0
func TestSummarize_EquivalentToClaudeWhenNoCacheCreation(t *testing.T) {
	steps := []compute.StepData{
		{Input: 1000, CacheRead: 0, Output: 200},
		{Input: 0, CacheRead: 1200, Output: 200},
	}
	prices := compute.ModelPrices{Input: 1.0, CacheRead: 0.1, Output: 4.0}

	kimiRows := compute.ComputeIdeal(steps)
	claudeRows := compute.ComputeIdealClaude(steps, prices)

	kimiS := compute.Summarize(kimiRows, prices)
	claudeS := compute.SummarizeClaude(claudeRows, prices)

	if math.Abs(kimiS.Actual-claudeS.Actual) > 1e-9 {
		t.Errorf("Actual cost mismatch: kimi=%f claude=%f", kimiS.Actual, claudeS.Actual)
	}
	if math.Abs(kimiS.Ideal-claudeS.Ideal) > 1e-9 {
		t.Errorf("Ideal cost mismatch: kimi=%f claude=%f", kimiS.Ideal, claudeS.Ideal)
	}
	if kimiS.TotalCR != claudeS.TotalCR {
		t.Errorf("TotalCR mismatch: kimi=%d claude=%d", kimiS.TotalCR, claudeS.TotalCR)
	}
}

// S4: printDetailRowsClaude should not print the meaningless i_in column.
func TestComputeIdealClaude_IdealInRemoved(t *testing.T) {
	steps := []compute.StepData{
		{Input: 500, CacheCreation: 500, CacheRead: 0, Output: 100},
		{Input: 100, CacheCreation: 100, CacheRead: 500, Output: 100},
		{Input: 50, CacheCreation: 50, CacheRead: 800, Output: 100},
	}
	prices := compute.ModelPrices{Input: 5.5, CacheCreation: 6.75, CacheRead: 0.55, Output: 27.5}
	rows := compute.ComputeIdealClaude(steps, prices)
	if len(rows) != len(steps) {
		t.Fatalf("expected %d rows, got %d", len(steps), len(rows))
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	printDetailRows(rows, uniformDetailPricing(len(rows), "claude-test-model", prices), true)
	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "i_in") {
		t.Fatalf("printDetailRowsClaude still prints i_in column:\n%s", out)
	}
	if !strings.Contains(out, "i_cr") || !strings.Contains(out, "i_cc") {
		t.Fatalf("printDetailRowsClaude output missing expected Claude columns:\n%s", out)
	}
}

// D6: compute.IdealRow.Note() covers all branches correctly
func TestIdealRowNote_AllBranches(t *testing.T) {
	cases := []struct {
		row  compute.IdealRow
		want string
	}{
		{compute.IdealRow{IsCompact: true, Waste: 999}, "COMPACT"},
		{compute.IdealRow{Waste: 0}, "HIT"},
		{compute.IdealRow{Waste: 100, CacheRead: 2000}, "PARTIAL"},
		{compute.IdealRow{Waste: 100, CacheRead: 500}, "MISS"},
	}
	for _, tc := range cases {
		got := tc.row.Note()
		if got != tc.want {
			t.Errorf("IdealRow.Note() = %q want %q (row=%+v)", got, tc.want, tc.row)
		}
	}
}

// D6: compute.IdealRow.Note() covers all branches correctly (should be identical to compute.IdealRow)
func TestClaudeIdealRowNote_AllBranches(t *testing.T) {
	cases := []struct {
		row  compute.IdealRow
		want string
	}{
		{compute.IdealRow{IsCompact: true, Waste: 999}, "COMPACT"},
		{compute.IdealRow{Waste: 0, IdealCC: 0}, "HIT"},
		{compute.IdealRow{Waste: 100, CacheRead: 2000}, "PARTIAL"},
		{compute.IdealRow{Waste: 100, CacheRead: 500}, "MISS"},
	}
	for _, tc := range cases {
		got := tc.row.Note()
		if got != tc.want {
			t.Errorf("ClaudeIdealRow.Note() = %q want %q (row=%+v)", got, tc.want, tc.row)
		}
	}
}

// D3: Compact detection uses a named threshold constant.
func TestCompactDetection_UsesNamedThreshold(t *testing.T) {
	old := compute.CompactThresholdPct
	compute.CompactThresholdPct = 70
	defer func() { compute.CompactThresholdPct = old }()

	steps := []compute.StepData{
		{Input: 1000, CacheRead: 0, Output: 0},
		{Input: 699, CacheRead: 0, Output: 0},
	}
	rows := compute.ComputeIdeal(steps)
	if !rows[1].IsCompact {
		t.Fatal("step 1 should be compact below the 70% boundary")
	}

	steps[1].Input = 700
	rows = compute.ComputeIdeal(steps)
	if rows[1].IsCompact {
		t.Fatal("step 1 should not be compact at the 70% boundary")
	}

	claudeSteps := []compute.StepData{
		{Input: 1000, CacheCreation: 0, CacheRead: 0, Output: 0},
		{Input: 699, CacheCreation: 0, CacheRead: 0, Output: 0},
	}
	prices := compute.ModelPrices{Input: 5.5, CacheCreation: 6.75, CacheRead: 0.55, Output: 27.5}
	claudeRows := compute.ComputeIdealClaude(claudeSteps, prices)
	if !claudeRows[1].IsCompact {
		t.Fatal("Claude step 1 should be compact below the 70% boundary")
	}

	claudeSteps[1].Input = 700
	claudeRows = compute.ComputeIdealClaude(claudeSteps, prices)
	if claudeRows[1].IsCompact {
		t.Fatal("Claude step 1 should not be compact at the 70% boundary")
	}
}

// D15: piStepActualCost must match the inline cost formula used elsewhere
func TestPiStepActualCost_MatchesInlineFormula(t *testing.T) {
	step := compute.StepData{Input: 1000, CacheCreation: 500, CacheRead: 800, Output: 200}
	prices := compute.ModelPrices{Input: 0.95, CacheCreation: 1.0, CacheRead: 0.16, Output: 4.0}

	got := compute.PiStepActualCost(step, prices)
	want := float64(step.Input)*prices.Input/compute.TokensPerMillion +
		float64(step.CacheCreation)*prices.CacheCreation/compute.TokensPerMillion +
		float64(step.CacheRead)*prices.CacheRead/compute.TokensPerMillion +
		float64(step.Output)*prices.Output/compute.TokensPerMillion

	if math.Abs(got-want) > 1e-12 {
		t.Errorf("piStepActualCost = %f, inline formula = %f, diff = %e", got, want, got-want)
	}
}

// Cache-write TTL split: a step with only 5m-TTL cache writes (CacheCreation1h
// left at zero, as every un-resynced row and every non-Claude agent has it)
// must cost exactly what the pre-split formula produced.
func TestPiStepActualCost_5mOnly_MatchesPreSplitRate(t *testing.T) {
	step := compute.StepData{Input: 1000, CacheCreation: 500, CacheRead: 800, Output: 200}
	prices := compute.ModelPrices{Input: 0.95, CacheCreation: 1.0, CacheCreation1h: 1.9, CacheRead: 0.16, Output: 4.0}

	got := compute.PiStepActualCost(step, prices)
	want := float64(step.Input)*prices.Input/compute.TokensPerMillion +
		float64(step.CacheCreation)*prices.CacheCreation/compute.TokensPerMillion +
		float64(step.CacheRead)*prices.CacheRead/compute.TokensPerMillion +
		float64(step.Output)*prices.Output/compute.TokensPerMillion

	if math.Abs(got-want) > 1e-12 {
		t.Errorf("5m-only cost = %f, want pre-split formula %f", got, want)
	}
}

// Cache-write TTL split: a step whose cache writes are entirely 1h-TTL must
// price the whole CacheCreation total at the 1h rate (2x input), not the 5m
// rate.
func TestPiStepActualCost_1hOnly_UsesHourlyRate(t *testing.T) {
	step := compute.StepData{Input: 1000, CacheCreation: 500, CacheCreation1h: 500, CacheRead: 800, Output: 200}
	prices := compute.ModelPrices{Input: 0.95, CacheCreation: 1.0, CacheCreation1h: 1.9, CacheRead: 0.16, Output: 4.0}

	got := compute.PiStepActualCost(step, prices)
	want := float64(step.Input)*prices.Input/compute.TokensPerMillion +
		float64(step.CacheCreation1h)*prices.CacheCreation1h/compute.TokensPerMillion +
		float64(step.CacheRead)*prices.CacheRead/compute.TokensPerMillion +
		float64(step.Output)*prices.Output/compute.TokensPerMillion

	if math.Abs(got-want) > 1e-12 {
		t.Errorf("1h-only cost = %f, want %f (all CacheCreation at the 1h rate)", got, want)
	}
	// Sanity check against the old single-rate formula: pricing this step at
	// the 5m rate throughout would have undercounted it.
	old := float64(step.Input)*prices.Input/compute.TokensPerMillion +
		float64(step.CacheCreation)*prices.CacheCreation/compute.TokensPerMillion +
		float64(step.CacheRead)*prices.CacheRead/compute.TokensPerMillion +
		float64(step.Output)*prices.Output/compute.TokensPerMillion
	if got <= old {
		t.Errorf("1h-priced cost %f should exceed the old 5m-rate-only cost %f", got, old)
	}
}

// Cache-write TTL split: a step with both 5m and 1h cache writes must blend
// the two rates rather than pricing the total at either one alone.
func TestPiStepActualCost_MixedTTL_BlendsRates(t *testing.T) {
	step := compute.StepData{Input: 1000, CacheCreation: 500, CacheCreation1h: 300, CacheRead: 800, Output: 200}
	prices := compute.ModelPrices{Input: 0.95, CacheCreation: 1.0, CacheCreation1h: 1.9, CacheRead: 0.16, Output: 4.0}

	got := compute.PiStepActualCost(step, prices)
	cc5m := step.CacheCreation - step.CacheCreation1h // 200 tokens at the 5m rate
	want := float64(step.Input)*prices.Input/compute.TokensPerMillion +
		float64(cc5m)*prices.CacheCreation/compute.TokensPerMillion +
		float64(step.CacheCreation1h)*prices.CacheCreation1h/compute.TokensPerMillion +
		float64(step.CacheRead)*prices.CacheRead/compute.TokensPerMillion +
		float64(step.Output)*prices.Output/compute.TokensPerMillion

	if math.Abs(got-want) > 1e-12 {
		t.Errorf("mixed-TTL cost = %f, want blended %f", got, want)
	}
}

// D15: Summarize actual cost must equal sum of piStepActualCost per row
func TestSummarize_ActualMatchesSumOfStepCosts(t *testing.T) {
	steps := []compute.StepData{
		{Input: 1000, CacheRead: 200, Output: 300},
		{Input: 200, CacheRead: 900, Output: 150},
		{Input: 100, CacheRead: 1000, Output: 200},
	}
	prices := compute.ModelPrices{Input: 0.95, CacheRead: 0.16, Output: 4.0}

	var sumStepCosts float64
	for _, s := range steps {
		sumStepCosts += compute.PiStepActualCost(s, prices)
	}

	rows := compute.ComputeIdeal(steps)
	s := compute.Summarize(rows, prices)

	if math.Abs(s.Actual-sumStepCosts) > 1e-9 {
		t.Errorf("Summarize.Actual = %f, sum of step costs = %f", s.Actual, sumStepCosts)
	}
}

// Bug: printDetailRows used to take one compute.ModelPrices and price every
// row with it, so a session that switched models mid-run had every row
// priced at whichever model was passed in. A session must be priced per
// row, at each row's own model — and, since ComputeIdealClaude carries
// state forward step to step, the ideal computation itself must still run
// once over the whole continuous step order (see totalsByAgent in main.go,
// the reference implementation this mirrors) rather than being split by
// model first.
func TestComputeDetailRowCosts_MixedModelPricedPerRow(t *testing.T) {
	steps := []compute.StepData{
		{Input: 20000, Output: 1000}, // served by "model-a"
		{Input: 20000, Output: 1000}, // served by "model-b", same shape
	}
	modelA := compute.ModelPrices{Input: 3.0, Output: 15.0, CacheRead: 0.3}
	modelB := compute.ModelPrices{Input: 15.0, Output: 75.0, CacheRead: 1.5}

	// One continuous ideal-cache pass over the untouched session order, same
	// as printDetailRows' callers now do — splitting by model before this
	// call would drop step 2's carried-forward context from step 1.
	rows := compute.ComputeIdealClaude(steps, compute.ModelPrices{})

	pricing := []detailRowPrice{
		{Model: "model-a", Prices: modelA, Priced: true},
		{Model: "model-b", Prices: modelB, Priced: true},
	}

	costs, kinds, totals := computeDetailRowCosts(rows, pricing)

	if len(costs) != 2 {
		t.Fatalf("costs = %+v, want 2 entries", costs)
	}
	for i, k := range kinds {
		if k != detailRowCostRated {
			t.Errorf("row %d kind = %v, want detailRowCostRated", i, k)
		}
	}

	rowStep := func(r compute.IdealRow) compute.StepData {
		return compute.StepData{Input: r.Input, CacheCreation: r.CacheCreation, CacheCreation1h: r.CacheCreation1h, CacheRead: r.CacheRead, Output: r.Output}
	}
	wantRow0 := compute.PiStepActualCost(rowStep(rows[0]), modelA)
	wantRow1 := compute.PiStepActualCost(rowStep(rows[1]), modelB)
	if costs[0] != wantRow0 {
		t.Errorf("row 0 cost = %v, want %v (priced at model-a's own rate)", costs[0], wantRow0)
	}
	if costs[1] != wantRow1 {
		t.Errorf("row 1 cost = %v, want %v (priced at model-b's own rate)", costs[1], wantRow1)
	}

	sumRows := costs[0] + costs[1]

	// Regression guard: pricing every row at model-a alone, or at model-b
	// alone, must give a different total than pricing each row at its own
	// model — otherwise this fixture wouldn't distinguish per-row pricing
	// from the old single-price bug.
	allAtA := compute.Summarize(rows, modelA).Actual
	allAtB := compute.Summarize(rows, modelB).Actual
	if sumRows == allAtA {
		t.Errorf("per-row sum %v equals pricing everything at model-a (%v); fixture doesn't distinguish per-row pricing", sumRows, allAtA)
	}
	if sumRows == allAtB {
		t.Errorf("per-row sum %v equals pricing everything at model-b (%v); fixture doesn't distinguish per-row pricing", sumRows, allAtB)
	}

	// totals.InCost/OutCost/etc are the categorized "$" line; they must add
	// up to the same per-row sum computed above, at the categories' own
	// per-row rates.
	if math.Abs((totals.InCost+totals.CCCost+totals.CRCost+totals.OutCost)-sumRows) > 1e-9 {
		t.Errorf("categorized totals sum = %v, want %v (sum of per-row costs)", totals.InCost+totals.CCCost+totals.CRCost+totals.OutCost, sumRows)
	}
	if totals.LoggedRows != 0 || totals.UnknownRows != 0 {
		t.Errorf("totals = %+v, want no logged/unknown rows (every row priced)", totals)
	}
}

// A row with no resolvable rate must not be dropped or priced at some other
// row's rate — it falls back to the agent's own logged cost, same
// preference order as totalsByAgent.
func TestDetailRowCost_FallsBackToLoggedCostWhenUnpriced(t *testing.T) {
	row := compute.IdealRow{Input: 5000, CacheRead: 1000, Output: 500}
	p := detailRowPrice{Model: "retired-model", Priced: false, LoggedCost: 3.25}

	cost, kind := detailRowCost(row, p)
	if kind != detailRowCostLogged {
		t.Errorf("kind = %v, want detailRowCostLogged", kind)
	}
	if cost != 3.25 {
		t.Errorf("cost = %v, want 3.25 (the logged cost)", cost)
	}
}

// A row with neither a resolvable rate nor a logged cost has a genuinely
// unknown price — it must read as unknown, not as a fabricated $0.00.
func TestDetailRowCost_UnknownWhenNoRateAndNoLoggedCost(t *testing.T) {
	row := compute.IdealRow{Input: 5000, CacheRead: 1000, Output: 500}
	p := detailRowPrice{Model: "retired-model", Priced: false, LoggedCost: 0}

	cost, kind := detailRowCost(row, p)
	if kind != detailRowCostUnknown {
		t.Errorf("kind = %v, want detailRowCostUnknown", kind)
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0", cost)
	}
}

// formatDetailRowCost must mark a logged-cost fallback and an unknown cost
// differently from a genuine rate-priced figure, so neither is mistaken for
// a number this program actually computed from a rate table.
func TestFormatDetailRowCost_MarksLoggedAndUnknown(t *testing.T) {
	if got := formatDetailRowCost(1.5, detailRowCostRated); got != "$1.5000" {
		t.Errorf("rated = %q, want %q", got, "$1.5000")
	}
	if got := formatDetailRowCost(1.5, detailRowCostLogged); got != "$1.5000~" {
		t.Errorf("logged = %q, want %q", got, "$1.5000~")
	}
	if got := formatDetailRowCost(0, detailRowCostUnknown); got != "n/a" {
		t.Errorf("unknown = %q, want %q", got, "n/a")
	}
}

// The printed table must name the model each row actually used — a table
// that silently mixes models without saying so is the bug being fixed.
func TestPrintDetailRows_ShowsPerRowModel(t *testing.T) {
	steps := []compute.StepData{
		{Input: 20000, Output: 1000},
		{Input: 20000, Output: 1000},
	}
	rows := compute.ComputeIdealClaude(steps, compute.ModelPrices{})
	pricing := []detailRowPrice{
		{Model: "model-cheap", Prices: compute.ModelPrices{Input: 1, Output: 2, CacheRead: 0.1}, Priced: true},
		{Model: "model-pricey", Prices: compute.ModelPrices{Input: 20, Output: 40, CacheRead: 2}, Priced: true},
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	printDetailRows(rows, pricing, true)
	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "model-cheap") {
		t.Errorf("output missing model-cheap:\n%s", out)
	}
	if !strings.Contains(out, "model-pricey") {
		t.Errorf("output missing model-pricey:\n%s", out)
	}
}

// Bug: computeDetailRowCosts' categorized "$" totals must add up to the sum
// of the per-row "$" costs even when rows carry real CacheCreation tokens.
// Before this fix that only mattered for the showCC=true (Claude) table,
// which prints a CCCost column; the showCC=false (OpenCode) table's footer
// dropped CCCost from its printed sum entirely even though every row's own
// "$" figure (detailRowCost -> compute.PiStepActualCost) already priced the
// full step including its cache-creation term. This test guards the
// underlying arithmetic that printDetailRows' footer must reflect regardless
// of which branch renders it.
func TestComputeDetailRowCosts_CategoriesSumToRowCosts_WithCacheCreation(t *testing.T) {
	steps := []compute.StepData{
		{Input: 2, CacheCreation: 20000, Output: 265},
		{Input: 1, CacheCreation: 5000, CacheRead: 11911, Output: 1310},
		{Input: 1, CacheCreation: 2000, CacheRead: 15617, Output: 588},
	}
	prices := compute.ModelPrices{Input: 15, CacheCreation: 18.75, CacheRead: 1.5, Output: 75}
	rows := compute.ComputeIdealClaude(steps, prices)
	pricing := uniformDetailPricing(len(rows), "claude-opus-test", prices)

	costs, kinds, totals := computeDetailRowCosts(rows, pricing)

	var sumRows float64
	for i, c := range costs {
		if kinds[i] != detailRowCostRated {
			t.Fatalf("row %d kind = %v, want detailRowCostRated", i, kinds[i])
		}
		sumRows += c
	}
	if sumRows == 0 {
		t.Fatal("fixture produced zero total cost; test doesn't exercise anything")
	}
	if totals.CCCost == 0 {
		t.Fatal("fixture produced zero CCCost; test doesn't exercise the cache-creation term")
	}

	categorized := totals.CRCost + totals.CCCost + totals.InCost + totals.OutCost
	if diff := categorized - sumRows; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("categorized totals (CRCost+CCCost+InCost+OutCost) = %v, want %v (sum of per-row $ costs); "+
			"a reader summing the printed $ column could not reach this footer", categorized, sumRows)
	}
}

// Bug: printDetailRows(rows, pricing, showCC=false) — the call ocDetail makes
// — used to trust the caller's showCC=false unconditionally, so a session
// whose steps genuinely wrote to cache (OpenCode's cache_write, selected as
// CacheCreation) got the narrow table: no c.write column, and a footer "$"
// line that summed only CRCost+InCost+OutCost. The row's own "$" figure
// still included the omitted cache-creation cost, so the column could never
// be summed to the footer. printDetailRows must widen the table itself once
// any row actually carries CacheCreation, regardless of what the caller
// believed when it decided showCC.
func TestPrintDetailRows_WidensTable_WhenCallerPassesShowCCFalseButRowsHaveCacheCreation(t *testing.T) {
	steps := []compute.StepData{
		{Input: 2, CacheCreation: 20000, Output: 265},
		{Input: 1, CacheCreation: 5000, CacheRead: 11911, Output: 1310},
		{Input: 1, CacheCreation: 2000, CacheRead: 15617, Output: 588},
	}
	prices := compute.ModelPrices{Input: 15, CacheCreation: 18.75, CacheRead: 1.5, Output: 75}
	rows := compute.ComputeIdealClaude(steps, prices)
	pricing := uniformDetailPricing(len(rows), "claude-opus-test", prices)

	_, _, totals := computeDetailRowCosts(rows, pricing)
	if totals.CCCost == 0 {
		t.Fatal("fixture produced zero CCCost; test doesn't exercise the cache-creation term")
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	// showCC=false mirrors ocDetail's call (opencode.go), which never checks
	// whether the session actually wrote to cache before deciding the table
	// shape.
	printDetailRows(rows, pricing, false)
	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "c.write") {
		t.Fatalf("printDetailRows(showCC=false) did not widen the table for rows with real CacheCreation tokens:\n%s", out)
	}
	ccCostStr := fmt.Sprintf("%.2f", totals.CCCost)
	if !strings.Contains(out, ccCostStr) {
		t.Errorf("footer is missing the CCCost figure (%s) that the omitted-column table used to drop:\n%s", ccCostStr, out)
	}
}
