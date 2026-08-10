package compute_test

import (
	"testing"

	"tokeneks/compute"
)

// TestComputeIdealClaude_CarryForwardZeroesIdealInAfterFirstStep states the
// invariant the carry-forward bug violated: in a conversation with no cache
// writes, where each step's context is exactly the previous step's total
// context plus that step's output (i.e. a perfectly linear, ever-growing
// transcript with no compaction), a perfectly-cached run would never need to
// re-read anything as fresh input after the first step. Before the fix,
// idealCR was carried forward as `idealCR + s.Output`, silently dropping
// idealIn every step — so every step after the first kept reporting the
// newly-read tokens as IdealIn instead of recognizing them as already
// covered by the (higher) carried idealCR. That is what turned OC's Ideal
// column into a number larger than Paid.
func TestComputeIdealClaude_CarryForwardZeroesIdealInAfterFirstStep(t *testing.T) {
	steps := []compute.StepData{
		{Input: 1000, Output: 200},
	}
	// Each subsequent step's Input is exactly "everything context-wise that
	// existed before" (previous Input/CacheRead total + previous Output),
	// with CacheRead left at 0 — i.e. the real system did zero caching, so
	// this isolates what an ideal cache *would* have done.
	prevTotal := steps[0].Input + steps[0].Output
	for i := 0; i < 4; i++ {
		steps = append(steps, compute.StepData{Input: prevTotal, Output: 150})
		prevTotal += 150
	}

	rows := compute.ComputeIdealClaude(steps, compute.ModelPrices{})

	if rows[0].IdealIn != steps[0].Input {
		t.Fatalf("step 0: IdealIn = %d, want %d (nothing to cache yet)", rows[0].IdealIn, steps[0].Input)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].IdealIn != 0 {
			t.Errorf("step %d: IdealIn = %d, want 0 (this step's whole context was covered by the carried-forward idealCR)", i, rows[i].IdealIn)
		}
	}
}

// TestSummarize_IdealNeverExceedsActual_MonotonicGrowth is the property
// whose violation started this investigation: `tokeneks total` printed an
// Ideal cost (773.37) larger than what was actually Paid (179.20) for
// OpenCode, which is impossible — a perfect cache can only ever be cheaper
// than, or as cheap as, reality, never more expensive. This builds a
// realistic multi-step session (context growing every step, no compaction,
// imperfect real-world caching — some fraction of each step's context comes
// back as fresh Input rather than a cache hit) and checks the invariant
// holds for the whole session, not just per step.
func TestSummarize_IdealNeverExceedsActual_MonotonicGrowth(t *testing.T) {
	steps := []compute.StepData{
		{Input: 2000, CacheRead: 0, Output: 300},   // cold start
		{Input: 500, CacheRead: 1800, Output: 250}, // ctx=2300, 500 missed the cache
		{Input: 200, CacheRead: 2350, Output: 200}, // ctx=2550, 200 missed
		{Input: 150, CacheRead: 2600, Output: 180}, // ctx=2750, 150 missed
		{Input: 300, CacheRead: 2630, Output: 220}, // ctx=2930, 300 missed
	}
	prices := compute.ModelPrices{Input: 3.0, CacheRead: 0.3, CacheCreation: 3.75, Output: 15.0}

	rows := compute.ComputeIdealClaude(steps, prices)
	s := compute.Summarize(rows, prices)

	if s.Ideal > s.Actual {
		t.Fatalf("Ideal ($%.4f) > Actual ($%.4f): a perfect cache cannot cost more than what was actually paid", s.Ideal, s.Actual)
	}
}

// TestComputeIdealClaude_CacheWriteCountsTowardNextStepContext is bug 2: a
// step that writes to the cache is adding real tokens to the conversation's
// context, and the next step should get credit for them via IdealCR, the
// same as it would for plain Input. The old ComputeIdeal (used by OpenCode)
// computed totalCtx as Input+CacheRead only, silently discarding
// CacheCreation, so those tokens never entered the running estimate at all.
func TestComputeIdealClaude_CacheWriteCountsTowardNextStepContext(t *testing.T) {
	steps := []compute.StepData{
		{Input: 200, CacheCreation: 500, Output: 100},
		{CacheRead: 800, Output: 50},
	}

	rows := compute.ComputeIdealClaude(steps, compute.ModelPrices{})

	// Step 0: totalCtx = 200 + 500 = 700, all of it new (nothing carried in
	// yet), all of it flagged as a cache write (CacheCreation > 0) rather
	// than IdealIn. Carry forward = 0 (prior idealCR) + idealIn(0) +
	// idealCC(700) + Output(100) = 800.
	if rows[0].IdealCC != 700 {
		t.Fatalf("step 0: IdealCC = %d, want 700 (Input 200 + CacheCreation 500, all newly written)", rows[0].IdealCC)
	}

	// Step 1's whole context (CacheRead: 800) is exactly what step 0 wrote
	// to the cache (700) plus what it output (100). If CacheCreation had
	// been dropped from the running estimate (the bug), IdealCR here would
	// only be 100 (just the carried Output), and the 700 written tokens
	// would incorrectly show up as fresh IdealIn instead.
	if rows[1].IdealCR != 800 {
		t.Fatalf("step 1: IdealCR = %d, want 800 (step 0's CacheCreation(500)+Input(200) plus its Output(100) all carried forward)", rows[1].IdealCR)
	}
	if rows[1].IdealIn != 0 {
		t.Fatalf("step 1: IdealIn = %d, want 0 (its entire context was covered by step 0's carried-forward writes)", rows[1].IdealIn)
	}
}

// TestComputeIdealClaude_CompactionResetsRunningEstimate preserves existing
// behavior: when a step's context shrinks below CompactThresholdPct of the
// previous step's context (a compaction/summarization event), the running
// idealCR estimate is not carried through — it is reset to that step's own
// CacheRead, because whatever got summarized away can no longer be assumed
// perfectly cacheable going forward.
func TestComputeIdealClaude_CompactionResetsRunningEstimate(t *testing.T) {
	steps := []compute.StepData{
		{Input: 5000, Output: 0},      // ctx=5000
		{Input: 100, CacheRead: 5000}, // ctx=5100, carries idealCR=5100 forward
		{Input: 200, CacheRead: 300},  // ctx=500 — well under 80% of 5100: compaction
	}

	rows := compute.ComputeIdealClaude(steps, compute.ModelPrices{})

	if !rows[2].IsCompact {
		t.Fatal("step 2: IsCompact = false, want true (ctx 500 is under 80% of the previous step's 5100)")
	}
	// The reset behavior in the code sets idealCR = s.CacheRead on
	// compaction, discarding the ~5100 carried in from before the
	// compaction rather than clipping or blending it.
	if rows[2].IdealCR != 300 {
		t.Fatalf("step 2: IdealCR = %d, want 300 (reset to this step's own CacheRead on compaction, not the carried-forward 5100)", rows[2].IdealCR)
	}
}

// TestComputeIdealClaude_HandComputedSequence is a table-driven regression
// over a short, fully hand-computed sequence, so a future reader can check
// the arithmetic in this comment against the assertions rather than trusting
// whatever the code currently produces.
//
// Step 0: Input=1000, CacheCreation=0, CacheRead=0, Output=200
//
//	totalCtx = 1000. idealCR starts at 0 (first step). newTokens = 1000.
//	No cache write this step, so IdealIn=1000, IdealCC=0. Waste = 0-0 = 0.
//	Carry forward: idealCR = 0 + 1000 + 0 + 200 = 1200.
//
// Step 1: Input=0, CacheCreation=300, CacheRead=1200, Output=150
//
//	totalCtx = 0+300+1200 = 1500. Not a compaction (1500 is not < 80% of
//	step 0's 1000). idealCR carried in = 1200 (<= totalCtx, no clip).
//	newTokens = 1500-1200 = 300. This step wrote to the cache, so
//	IdealCC=300, IdealIn=0. Waste = 1200-1200 = 0.
//	Carry forward: idealCR = 1200 + 0 + 300 + 150 = 1650.
//
// Step 2: Input=100, CacheCreation=0, CacheRead=1400, Output=100
//
//	totalCtx = 100+0+1400 = 1500. Not a compaction (1500 is not < 80% of
//	step 1's 1500). idealCR carried in = 1650, which exceeds totalCtx(1500)
//	so it's clipped down to 1500. newTokens = 1500-1500 = 0, so
//	IdealIn=IdealCC=0. Waste = 1500-1400 = 100 (the model claims a smaller
//	cache read than an ideal cache would have delivered).
func TestComputeIdealClaude_HandComputedSequence(t *testing.T) {
	steps := []compute.StepData{
		{Input: 1000, Output: 200},
		{CacheCreation: 300, CacheRead: 1200, Output: 150},
		{Input: 100, CacheRead: 1400, Output: 100},
	}

	want := []compute.IdealRow{
		{IdealCR: 0, IdealCC: 0, IdealIn: 1000, Waste: 0},
		{IdealCR: 1200, IdealCC: 300, IdealIn: 0, Waste: 0},
		{IdealCR: 1500, IdealCC: 0, IdealIn: 0, Waste: 100},
	}

	rows := compute.ComputeIdealClaude(steps, compute.ModelPrices{})
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		got := rows[i]
		if got.IdealCR != w.IdealCR || got.IdealCC != w.IdealCC || got.IdealIn != w.IdealIn || got.Waste != w.Waste {
			t.Errorf("step %d: got {IdealCR:%d IdealCC:%d IdealIn:%d Waste:%d}, want {IdealCR:%d IdealCC:%d IdealIn:%d Waste:%d}",
				i, got.IdealCR, got.IdealCC, got.IdealIn, got.Waste, w.IdealCR, w.IdealCC, w.IdealIn, w.Waste)
		}
	}
}
