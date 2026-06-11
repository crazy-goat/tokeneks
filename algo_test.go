package main

import (
	"math"
	"testing"
)

// D3: ComputeIdeal and ComputeIdealClaude must agree when CacheCreation == 0
func TestComputeIdeal_EquivalentToClaudeWhenNoCacheCreation(t *testing.T) {
	steps := []StepData{
		{Input: 1000, CacheRead: 0, Output: 200},
		{Input: 0, CacheRead: 1200, Output: 200},
		{Input: 200, CacheRead: 1000, Output: 150},
	}
	prices := ModelPrices{Input: 1.0, CacheRead: 0.1, Output: 4.0}

	kimiRows := ComputeIdeal(steps)
	claudeRows := ComputeIdealClaude(steps, prices)

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
	rows := []IdealRow{}
	prices := ModelPrices{Input: 1.0, CacheRead: 0.1, Output: 4.0}

	s := Summarize(rows, prices)

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
	rows := []ClaudeIdealRow{}
	prices := ModelPrices{Input: 5.5, CacheCreation: 6.75, CacheRead: 0.55, Output: 27.5}

	s := SummarizeClaude(rows, prices)

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
	steps := []StepData{
		{Input: 1000, CacheRead: 0, Output: 200},
		{Input: 0, CacheRead: 1200, Output: 200},
	}
	prices := ModelPrices{Input: 1.0, CacheRead: 0.1, Output: 4.0}

	kimiRows := ComputeIdeal(steps)
	claudeRows := ComputeIdealClaude(steps, prices)

	kimiS := Summarize(kimiRows, prices)
	claudeS := SummarizeClaude(claudeRows, prices)

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

// S4: IdealIn is always 0 in ComputeIdealClaude — documents this as a known structural issue
func TestComputeIdealClaude_IdealInRemoved(t *testing.T) {
	steps := []StepData{
		{Input: 500, CacheCreation: 500, CacheRead: 0, Output: 100},
		{Input: 100, CacheCreation: 100, CacheRead: 500, Output: 100},
		{Input: 50, CacheCreation: 50, CacheRead: 800, Output: 100},
	}
	prices := ModelPrices{Input: 5.5, CacheCreation: 6.75, CacheRead: 0.55, Output: 27.5}
	rows := ComputeIdealClaude(steps, prices)
	// IdealIn was always 0 for Claude; it has been removed from ClaudeIdealRow.
	// Verify that the output format still works correctly (no panic, sensible values).
	if len(rows) != len(steps) {
		t.Fatalf("expected %d rows, got %d", len(steps), len(rows))
	}
	for i, r := range rows {
		if r.IdealCR < 0 {
			t.Errorf("step %d: IdealCR=%d, expected >= 0", i, r.IdealCR)
		}
	}
}

// D6: IdealRow.Note() covers all branches correctly
func TestIdealRowNote_AllBranches(t *testing.T) {
	cases := []struct {
		row  IdealRow
		want string
	}{
		{IdealRow{IsCompact: true, Waste: 999}, "COMPACT"},
		{IdealRow{Waste: 0}, "HIT"},
		{IdealRow{Waste: 100, CacheRead: 2000}, "PARTIAL"},
		{IdealRow{Waste: 100, CacheRead: 500}, "MISS"},
	}
	for _, tc := range cases {
		got := tc.row.Note()
		if got != tc.want {
			t.Errorf("IdealRow.Note() = %q want %q (row=%+v)", got, tc.want, tc.row)
		}
	}
}

// D6: ClaudeIdealRow.Note() covers all branches correctly (should be identical to IdealRow)
func TestClaudeIdealRowNote_AllBranches(t *testing.T) {
	cases := []struct {
		row  ClaudeIdealRow
		want string
	}{
		{ClaudeIdealRow{IsCompact: true, Waste: 999}, "COMPACT"},
		{ClaudeIdealRow{Waste: 0, IdealCC: 0}, "HIT"},
		{ClaudeIdealRow{Waste: 100, CacheRead: 2000}, "PARTIAL"},
		{ClaudeIdealRow{Waste: 100, CacheRead: 500}, "MISS"},
	}
	for _, tc := range cases {
		got := tc.row.Note()
		if got != tc.want {
			t.Errorf("ClaudeIdealRow.Note() = %q want %q (row=%+v)", got, tc.want, tc.row)
		}
	}
}

// D3: Compact detection threshold (80%) must be consistent in both compute functions
func TestCompactDetection_80PercentThreshold(t *testing.T) {
	// step[1] with totalCtx 799 < 80% of step[0] totalCtx 1000 => compact
	steps := []StepData{
		{Input: 1000, CacheRead: 0, Output: 0},
		{Input: 799, CacheRead: 0, Output: 0},
	}
	rows := ComputeIdeal(steps)
	if !rows[1].IsCompact {
		t.Error("step 1 should be compact: 799 < 80% of 1000")
	}

	// exactly 800 = 80% of 1000 => NOT compact
	steps[1].Input = 800
	rows = ComputeIdeal(steps)
	if rows[1].IsCompact {
		t.Error("step 1 should NOT be compact: 800 == 80% of 1000 (boundary)")
	}
}

func TestCompactDetection_80PercentThreshold_Claude(t *testing.T) {
	steps := []StepData{
		{Input: 1000, CacheCreation: 0, CacheRead: 0, Output: 0},
		{Input: 799, CacheCreation: 0, CacheRead: 0, Output: 0},
	}
	prices := ModelPrices{Input: 5.5, CacheCreation: 6.75, CacheRead: 0.55, Output: 27.5}
	rows := ComputeIdealClaude(steps, prices)
	if !rows[1].IsCompact {
		t.Error("step 1 should be compact in Claude path: 799 < 80% of 1000")
	}
}

// D15: piStepActualCost must match the inline cost formula used elsewhere
func TestPiStepActualCost_MatchesInlineFormula(t *testing.T) {
	step := StepData{Input: 1000, CacheCreation: 500, CacheRead: 800, Output: 200}
	prices := ModelPrices{Input: 0.95, CacheCreation: 1.0, CacheRead: 0.16, Output: 4.0}

	got := piStepActualCost(step, prices)
	want := float64(step.Input)*prices.Input/1e6 +
		float64(step.CacheCreation)*prices.CacheCreation/1e6 +
		float64(step.CacheRead)*prices.CacheRead/1e6 +
		float64(step.Output)*prices.Output/1e6

	if math.Abs(got-want) > 1e-12 {
		t.Errorf("piStepActualCost = %f, inline formula = %f, diff = %e", got, want, got-want)
	}
}

// D15: Summarize actual cost must equal sum of piStepActualCost per row
func TestSummarize_ActualMatchesSumOfStepCosts(t *testing.T) {
	steps := []StepData{
		{Input: 1000, CacheRead: 200, Output: 300},
		{Input: 200, CacheRead: 900, Output: 150},
		{Input: 100, CacheRead: 1000, Output: 200},
	}
	prices := ModelPrices{Input: 0.95, CacheRead: 0.16, Output: 4.0}

	var sumStepCosts float64
	for _, s := range steps {
		sumStepCosts += piStepActualCost(s, prices)
	}

	rows := ComputeIdeal(steps)
	s := Summarize(rows, prices)

	if math.Abs(s.Actual-sumStepCosts) > 1e-9 {
		t.Errorf("Summarize.Actual = %f, sum of step costs = %f", s.Actual, sumStepCosts)
	}
}
