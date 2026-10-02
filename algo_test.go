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
	_ = w.Close()
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
	_ = w.Close()
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
	_ = w.Close()
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

// computeDetailCostRecon: every row priced and every row logged (full
// coverage), at a deliberately large logged/rate ratio so a bug that fell
// back to rate (or vice versa) would show up as an obviously wrong total
// rather than a rounding difference. With full coverage CoveredRatedTotal
// must equal RatedTotal exactly — the two denominators agree when there's
// nothing uncovered to dilute one of them.
func TestComputeDetailCostRecon_AllRowsLogged(t *testing.T) {
	rows := []compute.IdealRow{
		{Input: 1_000_000, Output: 100_000},
		{Input: 2_000_000, Output: 200_000},
	}
	prices := compute.ModelPrices{Input: 0.95, Output: 4.0}
	pricing := []detailRowPrice{
		{Model: "m", Prices: prices, Priced: true, LoggedCost: 10},
		{Model: "m", Prices: prices, Priced: true, LoggedCost: 20},
	}

	r := computeDetailCostRecon(rows, pricing)

	wantRated := compute.PiStepActualCost(compute.StepData{Input: rows[0].Input, Output: rows[0].Output}, prices) +
		compute.PiStepActualCost(compute.StepData{Input: rows[1].Input, Output: rows[1].Output}, prices)
	if r.RatedTotal != wantRated {
		t.Errorf("RatedTotal = %v, want %v", r.RatedTotal, wantRated)
	}
	if r.CoveredRatedTotal != wantRated {
		t.Errorf("CoveredRatedTotal = %v, want %v (full coverage: covered subset is the whole session)", r.CoveredRatedTotal, wantRated)
	}
	if r.LoggedTotal != 30 {
		t.Errorf("LoggedTotal = %v, want 30 (sum of logged costs)", r.LoggedTotal)
	}
	if r.CoveredRows != 2 || r.PricedRows != 2 {
		t.Errorf("CoveredRows=%d PricedRows=%d, want 2 and 2", r.CoveredRows, r.PricedRows)
	}
	if got := r.Delta(); got != 30-wantRated {
		t.Errorf("Delta() = %v, want %v", got, 30-wantRated)
	}
	// Full coverage: dividing by RatedTotal or CoveredRatedTotal must give
	// the identical percentage, since they're the same number here.
	if got, naive := r.PctDelta(), r.Delta()/r.RatedTotal*100; got != naive {
		t.Errorf("PctDelta() = %v, want %v (RatedTotal and CoveredRatedTotal agree under full coverage)", got, naive)
	}
}

// computeDetailCostRecon: a row with no logged cost must fall back to its
// own rate cost inside LoggedTotal (the same preference ocSessionSummary's
// headline applies), and must not count toward CoveredRows or
// CoveredRatedTotal — the covered fraction and covered-subset denominator
// the task asks be reported for partial logged coverage.
func TestComputeDetailCostRecon_PartialLoggedCoverage(t *testing.T) {
	rows := []compute.IdealRow{
		{Input: 1_000_000, Output: 100_000}, // logged
		{Input: 1_000_000, Output: 100_000}, // not logged -> falls back to rate
	}
	prices := compute.ModelPrices{Input: 0.95, Output: 4.0}
	pricing := []detailRowPrice{
		{Model: "m", Prices: prices, Priced: true, LoggedCost: 50},
		{Model: "m", Prices: prices, Priced: true, LoggedCost: 0},
	}

	r := computeDetailCostRecon(rows, pricing)

	rateEach := compute.PiStepActualCost(compute.StepData{Input: 1_000_000, Output: 100_000}, prices)
	wantLoggedTotal := 50 + rateEach // row 0 logged, row 1 falls back to its own rate
	if r.LoggedTotal != wantLoggedTotal {
		t.Errorf("LoggedTotal = %v, want %v (logged row 0 + rate-fallback row 1)", r.LoggedTotal, wantLoggedTotal)
	}
	if r.RatedTotal != 2*rateEach {
		t.Errorf("RatedTotal = %v, want %v (both rows at their own rate)", r.RatedTotal, 2*rateEach)
	}
	if r.CoveredRatedTotal != rateEach {
		t.Errorf("CoveredRatedTotal = %v, want %v (only row 0's rate — the only row that logged a cost)", r.CoveredRatedTotal, rateEach)
	}
	if r.CoveredRows != 1 {
		t.Errorf("CoveredRows = %d, want 1 (only row 0 carried a logged cost)", r.CoveredRows)
	}
	if r.PricedRows != 2 {
		t.Errorf("PricedRows = %d, want 2 (both rows had a resolvable rate)", r.PricedRows)
	}
	// The whole point of the fix: with only half the rated total covered,
	// dividing by CoveredRatedTotal instead of the full RatedTotal must give
	// a visibly different (larger) percentage.
	coveredPct := r.PctDelta()
	wholeSessionPct := r.Delta() / r.RatedTotal * 100
	if coveredPct == wholeSessionPct {
		t.Fatalf("fixture doesn't distinguish the two denominators: covered=%v whole-session=%v", coveredPct, wholeSessionPct)
	}
	if math.Abs(coveredPct) <= math.Abs(wholeSessionPct) {
		t.Errorf("PctDelta() = %v, want a larger magnitude than the whole-session %v (covered denominator is smaller)", coveredPct, wholeSessionPct)
	}
}

// computeDetailCostRecon: a row with no resolvable rate must be excluded
// from every total entirely (mirroring ocSessionSummary's unpriced-row
// exclusion), even if it happens to carry a logged cost — there is no rate
// figure to compare that logged cost against, so it can't contribute to
// CoveredRatedTotal either.
func TestComputeDetailCostRecon_UnpricedRowExcluded(t *testing.T) {
	rows := []compute.IdealRow{
		{Input: 1_000_000, Output: 100_000}, // priced
		{Input: 5_000_000, Output: 500_000}, // unpriced, but logged
	}
	prices := compute.ModelPrices{Input: 0.95, Output: 4.0}
	pricing := []detailRowPrice{
		{Model: "m", Prices: prices, Priced: true, LoggedCost: 0},
		{Model: "retired-model", Priced: false, LoggedCost: 99},
	}

	r := computeDetailCostRecon(rows, pricing)

	wantRated := compute.PiStepActualCost(compute.StepData{Input: rows[0].Input, Output: rows[0].Output}, prices)
	if r.RatedTotal != wantRated {
		t.Errorf("RatedTotal = %v, want %v (unpriced row's tokens excluded)", r.RatedTotal, wantRated)
	}
	if r.LoggedTotal != wantRated {
		t.Errorf("LoggedTotal = %v, want %v (unpriced row's $99 logged cost excluded)", r.LoggedTotal, wantRated)
	}
	if r.PricedRows != 1 {
		t.Errorf("PricedRows = %d, want 1", r.PricedRows)
	}
	if r.CoveredRows != 0 {
		t.Errorf("CoveredRows = %d, want 0 (the only logged row has no rate to compare it against)", r.CoveredRows)
	}
	if r.CoveredRatedTotal != 0 {
		t.Errorf("CoveredRatedTotal = %v, want 0 (no row is both priced and logged)", r.CoveredRatedTotal)
	}
}

// detailCostRecon.PctDelta must divide by CoveredRatedTotal — the
// rate-derived total of just the rows that logged a cost — not RatedTotal,
// the whole-session rate-derived total. This mirrors a real session where
// only a small fraction of priced rows carried a logged cost: the two
// denominators give visibly different percentages, and CoveredRatedTotal
// is the one that actually measures drift on the rows the delta came from.
func TestDetailCostRecon_PctDelta_UsesCoveredSubsetNotWholeSession(t *testing.T) {
	r := detailCostRecon{
		RatedTotal:        1000.00, // whole-session rate-derived total
		CoveredRatedTotal: 100.00,  // rate-derived total of just the covered rows
		LoggedTotal:       1010.00, // same rows, logged cost swapped in for the covered ones
		CoveredRows:       10,
		PricedRows:        100,
	}
	if got, want := r.PctDelta(), 10.0; got != want {
		t.Errorf("PctDelta() = %v, want %v (10.00 delta / 100.00 covered rated total)", got, want)
	}
	if wrongPct := r.Delta() / r.RatedTotal * 100; wrongPct != 1.0 {
		t.Fatalf("fixture's whole-session-denominator sanity check failed: got %v, want 1.0", wrongPct)
	}
}

// detailCostRecon.Delta/PctDelta: PctDelta must be a safe 0, not NaN/Inf,
// when CoveredRatedTotal is 0 — the exact "zero denominator" case the task
// calls out. RatedTotal is deliberately left non-zero here to prove the
// guard checks CoveredRatedTotal specifically, not the whole-session total.
func TestDetailCostRecon_PctDelta_ZeroCoveredRatedTotal_IsZeroNotNaN(t *testing.T) {
	r := detailCostRecon{RatedTotal: 40, CoveredRatedTotal: 0, LoggedTotal: 45, CoveredRows: 1, PricedRows: 5}
	if got := r.PctDelta(); got != 0 {
		t.Errorf("PctDelta() = %v, want 0 when CoveredRatedTotal is 0", got)
	}
	if math.IsNaN(r.PctDelta()) || math.IsInf(r.PctDelta(), 0) {
		t.Errorf("PctDelta() = %v, want a finite number", r.PctDelta())
	}
}

// detailCostRecon.Significant: no row carried a logged cost at all — there
// is nothing to reconcile, regardless of what the totals happen to hold.
func TestDetailCostRecon_Significant_NoCoverage_IsFalse(t *testing.T) {
	r := detailCostRecon{RatedTotal: 10, CoveredRatedTotal: 0, LoggedTotal: 1000, CoveredRows: 0, PricedRows: 5}
	if r.Significant() {
		t.Error("Significant() = true, want false when CoveredRows == 0")
	}
}

// detailCostRecon.Significant: a delta below the half-cent absolute floor
// must not print, even though some row is logged — this is the "agreement
// within rounding" case the task says must be silent.
func TestDetailCostRecon_Significant_BelowAbsoluteFloor_IsFalse(t *testing.T) {
	r := detailCostRecon{RatedTotal: 10.000, CoveredRatedTotal: 10.000, LoggedTotal: 10.003, CoveredRows: 1, PricedRows: 1}
	if r.Significant() {
		t.Error("Significant() = true, want false for a $0.003 delta (below the half-cent floor)")
	}
}

// detailCostRecon.Significant: an absolute delta that clears the cent floor
// but is a negligible fraction of the covered subset must still not print —
// the percentage gate exists precisely so a big covered subset with a tiny
// relative drift doesn't get flagged.
func TestDetailCostRecon_Significant_BelowPercentFloor_IsFalse(t *testing.T) {
	r := detailCostRecon{RatedTotal: 1000.00, CoveredRatedTotal: 1000.00, LoggedTotal: 1000.40, CoveredRows: 1, PricedRows: 1}
	if r.PctDelta() >= detailReconMinPctDelta {
		t.Fatalf("fixture doesn't exercise the percent floor: PctDelta=%v", r.PctDelta())
	}
	if r.Significant() {
		t.Error("Significant() = true, want false when PctDelta is below detailReconMinPctDelta despite a >$0.005 delta")
	}
}

// detailCostRecon.Significant: a delta clearing both floors must print.
func TestDetailCostRecon_Significant_AboveBothFloors_IsTrue(t *testing.T) {
	r := detailCostRecon{RatedTotal: 10.00, CoveredRatedTotal: 10.00, LoggedTotal: 11.00, CoveredRows: 1, PricedRows: 1}
	if !r.Significant() {
		t.Error("Significant() = false, want true for a $1.00 / 10% delta")
	}
}

// detailCostRecon.Significant: an absolute delta exactly at the half-cent
// floor (not below it) must still count, provided the percentage floor is
// comfortably cleared — the check is "< floor => insignificant", so a delta
// equal to the floor must pass.
func TestDetailCostRecon_Significant_AtAbsoluteFloor_IsTrue(t *testing.T) {
	r := detailCostRecon{RatedTotal: 0.50, CoveredRatedTotal: 0.50, LoggedTotal: 0.505, CoveredRows: 1, PricedRows: 1}
	delta := r.Delta()
	if math.Abs(delta-detailReconMinAbsDelta) > 1e-9 {
		t.Fatalf("fixture doesn't sit at the absolute floor: delta=%v want=%v", delta, detailReconMinAbsDelta)
	}
	if !r.Significant() {
		t.Error("Significant() = false, want true: delta exactly equals detailReconMinAbsDelta, which must not be filtered")
	}
}

// detailCostRecon.Significant: a percentage exactly at the 0.5% floor (not
// below it) must still count, provided the absolute floor is comfortably
// cleared — the check is ">= floor => significant".
func TestDetailCostRecon_Significant_AtPercentFloor_IsTrue(t *testing.T) {
	r := detailCostRecon{RatedTotal: 100.00, CoveredRatedTotal: 100.00, LoggedTotal: 100.50, CoveredRows: 1, PricedRows: 1}
	if math.Abs(r.PctDelta()-detailReconMinPctDelta) > 1e-9 {
		t.Fatalf("fixture doesn't sit at the percent floor: PctDelta=%v want=%v", r.PctDelta(), detailReconMinPctDelta)
	}
	if !r.Significant() {
		t.Error("Significant() = false, want true: PctDelta exactly equals detailReconMinPctDelta, which must not be filtered")
	}
}

// detailCostRecon.Significant: CoveredRatedTotal == 0 (every covered row
// happens to rate-price at exactly $0) must not hide a real logged cost
// behind PctDelta's safe zero fallback — the percent check is skipped
// entirely in that case. RatedTotal is deliberately non-zero (other,
// uncovered rows do carry a rate) to prove the guard checks
// CoveredRatedTotal specifically, not the whole-session RatedTotal.
func TestDetailCostRecon_Significant_ZeroCoveredRatedTotal_NonzeroWholeSessionTotal_IsTrue(t *testing.T) {
	r := detailCostRecon{RatedTotal: 50.00, CoveredRatedTotal: 0, LoggedTotal: 51.00, CoveredRows: 1, PricedRows: 5}
	if !r.Significant() {
		t.Error("Significant() = false, want true: CoveredRatedTotal is 0 but a real $1.00 was logged, and RatedTotal alone is non-zero")
	}
}

// printDetailCostReconciliation must print nothing at all for an
// insignificant recon (no noise), and the labelled drift block for a
// significant one, naming it as drift rather than an error, with the
// percentage explicitly tied to the covered-subset total rather than the
// whole-session rate-derived total printed alongside it.
func TestPrintDetailCostReconciliation_SilentUnlessSignificant(t *testing.T) {
	silent := captureStdout(t, func() {
		printDetailCostReconciliation(detailCostRecon{RatedTotal: 10, CoveredRatedTotal: 0, LoggedTotal: 10, CoveredRows: 0, PricedRows: 3})
	})
	if silent != "" {
		t.Errorf("printDetailCostReconciliation printed output for an insignificant recon:\n%s", silent)
	}

	// RatedTotal (whole session) is deliberately much larger than
	// CoveredRatedTotal (the covered subset) so the naive "delta / whole
	// RatedTotal" percentage (10%) and the correct "delta / CoveredRatedTotal"
	// percentage (20%) are both plausible-looking but different — the
	// printed number must be the latter.
	loud := captureStdout(t, func() {
		printDetailCostReconciliation(detailCostRecon{RatedTotal: 10.00, CoveredRatedTotal: 5.00, LoggedTotal: 11.00, CoveredRows: 3, PricedRows: 5})
	})
	if !strings.Contains(loud, "Price-table drift") {
		t.Errorf("printDetailCostReconciliation output missing the drift label:\n%s", loud)
	}
	if !strings.Contains(loud, "not an error") {
		t.Errorf("printDetailCostReconciliation output must explicitly say this drift is not an error:\n%s", loud)
	}
	if !strings.Contains(loud, "3/5") {
		t.Errorf("printDetailCostReconciliation output missing the covered fraction (3/5):\n%s", loud)
	}
	if !strings.Contains(loud, "10.0000") || !strings.Contains(loud, "11.0000") {
		t.Errorf("printDetailCostReconciliation output missing both whole-session totals:\n%s", loud)
	}
	if !strings.Contains(loud, "5.0000") {
		t.Errorf("printDetailCostReconciliation output missing the covered-subset rate-derived total (5.0000):\n%s", loud)
	}
	if !strings.Contains(loud, "+20.0%") {
		t.Errorf("printDetailCostReconciliation output should report +20.0%% (1.00 delta / 5.00 covered total), not the whole-session-denominator +10.0%%:\n%s", loud)
	}
	if strings.Contains(loud, "+10.0%") {
		t.Errorf("printDetailCostReconciliation output must not report the stale whole-session-denominator percentage (+10.0%%):\n%s", loud)
	}
}
