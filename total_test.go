package main

import (
	"context"
	"testing"
	"time"
	"tokeneks/compute"
	"tokeneks/store"
)

// testStep is one assistant message to ingest: its tokens, the model that
// served it, and the cost its agent logged (0 = logged nothing).
type testStep struct {
	Model string
	Step  compute.StepData
	Cost  float64
}

// ingestTotalTestSession writes one session with one assistant message per
// step directly through the store's typed API, bypassing any agent parser.
// Each step carries its own model so tests can exercise per-step pricing
// without needing a real ~/.claude or ~/.pi session file on disk.
func ingestTotalTestSession(t *testing.T, st *store.Store, agent, sessionID string, steps []testStep) {
	t.Helper()
	now := time.Now().UnixMilli()
	msgs := make([]store.ParsedMessage, len(steps))
	for i, s := range steps {
		msgs[i] = store.ParsedMessage{Message: store.Message{
			Agent:        agent,
			SessionID:    sessionID,
			MsgIndex:     i,
			Role:         store.RoleAssistant,
			Model:        s.Model,
			InputTokens:  s.Step.Input,
			OutputTokens: s.Step.Output,
			CacheRead:    s.Step.CacheRead,
			CacheWrite:   s.Step.CacheCreation,
			CacheWrite1h: s.Step.CacheCreation1h,
			Cost:         s.Cost,
			CreatedAt:    now,
		}}
	}
	ps := store.ParsedSession{
		Session:  store.Session{Agent: agent, SessionID: sessionID, CreatedAt: now, LastActivity: now},
		Messages: msgs,
	}
	if err := st.IngestSession(context.Background(), ps); err != nil {
		t.Fatalf("IngestSession(%s/%s): %v", agent, sessionID, err)
	}
}

// step is a shorthand constructor to keep the test tables above readable.
// Cost stays 0, i.e. "the agent logged no cost for this message".
func step(model string, input, cacheRead, output int) testStep {
	return testStep{Model: model, Step: compute.StepData{Input: input, CacheRead: cacheRead, Output: output}}
}

// loggedStep is step() plus the cost the agent's own log reported, which
// totalsByAgent prefers over recomputing from a rate table.
func loggedStep(model string, input, cacheRead, output int, cost float64) testStep {
	s := step(model, input, cacheRead, output)
	s.Cost = cost
	return s
}

func agentTotalRow(t *testing.T, rows []agentTotal, label string) agentTotal {
	t.Helper()
	for _, r := range rows {
		if r.Label == label {
			return r
		}
	}
	t.Fatalf("no %s row in %+v", label, rows)
	return agentTotal{}
}

// Bug #2: a Claude session must show up in the total at all. Before the
// fix, printTotal only ever queried "opencode" and "pi" and Claude — the
// largest cost source in the store — contributed nothing.
func TestTotalsByAgent_ClaudeContributes(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "claude", "s1", []testStep{
		step("claude-sonnet-5", 10000, 0, 2000),
	})

	rows, unpriced, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}
	if len(unpriced) != 0 {
		t.Fatalf("unexpected unpriced models: %+v", unpriced)
	}

	claude := agentTotalRow(t, rows, "CLAUDE")
	prices := claudeGlobalModelPrices()["claude-sonnet-5"]
	want := compute.SummarizeClaude(
		compute.ComputeIdealClaude([]compute.StepData{{Input: 10000, Output: 2000}}, prices), prices)
	if claude.Actual != want.Actual {
		t.Errorf("CLAUDE actual = %v, want %v", claude.Actual, want.Actual)
	}
	if claude.Actual <= 0 {
		t.Errorf("CLAUDE actual = %v, want > 0", claude.Actual)
	}
}

// Bug #1: a PI session must be priced from the PI table, not from OC's
// ocModelPrices (which the old code applied to every agent).
func TestTotalsByAgent_PIUsesPITable(t *testing.T) {
	st := withTempStore(t)

	prevPrices := piPricesFunc
	fakePrices := map[string]compute.ModelPrices{
		// Deliberately far from any OC rate (Kimi K2.6 input is 0.95) so a
		// mix-up is obvious rather than coincidentally close.
		"test-pi-model": {Input: 42, Output: 84, CacheRead: 4.2},
	}
	piPricesFunc = func() map[string]compute.ModelPrices { return fakePrices }
	t.Cleanup(func() { piPricesFunc = prevPrices })

	ingestTotalTestSession(t, st, "pi", "s1", []testStep{
		step("test-pi-model", 5000, 1000, 500),
	})

	rows, unpriced, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}
	if len(unpriced) != 0 {
		t.Fatalf("unexpected unpriced models: %+v", unpriced)
	}

	pi := agentTotalRow(t, rows, "PI")
	want := compute.SummarizeClaude(
		compute.ComputeIdealClaude([]compute.StepData{{Input: 5000, CacheRead: 1000, Output: 500}}, fakePrices["test-pi-model"]),
		fakePrices["test-pi-model"])
	if pi.Actual != want.Actual {
		t.Errorf("PI actual = %v, want %v (priced from the PI table)", pi.Actual, want.Actual)
	}

	// Pricing the same tokens at Kimi's OC rate would give a different
	// number; if it happened to match, the test above wouldn't have caught
	// a table mix-up, so assert the two rates actually diverge.
	kimiPriced := compute.SummarizeClaude(
		compute.ComputeIdealClaude([]compute.StepData{{Input: 5000, CacheRead: 1000, Output: 500}}, ocModelPrices["Kimi K2.6"]),
		ocModelPrices["Kimi K2.6"])
	if want.Actual == kimiPriced.Actual {
		t.Fatalf("test fixture doesn't distinguish PI pricing from OC pricing")
	}
}

// Bug #4: sessions routinely switch models mid-run. A session must be priced
// per model, not wholly at the first assistant message's model — but the
// ideal-cache computation itself must still run over the whole, continuous
// session, or the model switch loses the first step's carried-forward
// context and quietly inflates the ideal side (a different route to the
// same "impossible" Paid<Ideal bug this whole feature exists to fix).
func TestTotalsByAgent_SessionSpanningTwoModelsPricedPerModel(t *testing.T) {
	st := withTempStore(t)

	steps := []compute.StepData{
		{Input: 20000, Output: 1000}, // served by claude-sonnet-5
		{Input: 20000, Output: 1000}, // served by claude-opus-5, same shape
	}
	ingestTotalTestSession(t, st, "claude", "s1", []testStep{
		{Model: "claude-sonnet-5", Step: steps[0]},
		{Model: "claude-opus-5", Step: steps[1]},
	})

	rows, _, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}
	claude := agentTotalRow(t, rows, "CLAUDE")

	sonnet := claudeGlobalModelPrices()["claude-sonnet-5"]
	opus := claudeGlobalModelPrices()["claude-opus-5"]

	// Correct: one continuous ComputeIdealClaude pass over both steps (so
	// step 2's ideal cache read still credits step 1's carried-forward
	// output), then each resulting row priced at its own model's rate.
	idealRows := compute.ComputeIdealClaude(steps, compute.ModelPrices{})
	wantActual := compute.PiStepActualCost(compute.StepData{Input: idealRows[0].Input, CacheRead: idealRows[0].CacheRead, Output: idealRows[0].Output}, sonnet) +
		compute.PiStepActualCost(compute.StepData{Input: idealRows[1].Input, CacheRead: idealRows[1].CacheRead, Output: idealRows[1].Output}, opus)
	wantIdeal := compute.PiStepActualCost(compute.StepData{Input: idealRows[0].IdealIn, CacheRead: idealRows[0].IdealCR, Output: idealRows[0].Output}, sonnet) +
		compute.PiStepActualCost(compute.StepData{Input: idealRows[1].IdealIn, CacheRead: idealRows[1].IdealCR, Output: idealRows[1].Output}, opus)

	if claude.Actual != wantActual {
		t.Errorf("CLAUDE actual = %v, want %v (per-row pricing on the continuous session)", claude.Actual, wantActual)
	}
	if claude.Ideal != wantIdeal {
		t.Errorf("CLAUDE ideal = %v, want %v (continuous ideal computation, priced per row)", claude.Ideal, wantIdeal)
	}

	// Regression guard 1: the old bug priced the whole session at the
	// first step's model instead of per-step.
	allAtSonnet := compute.SummarizeClaude(compute.ComputeIdealClaude(steps, sonnet), sonnet)
	if claude.Actual == allAtSonnet.Actual {
		t.Fatalf("test fixture doesn't distinguish per-model pricing from whole-session-at-first-model pricing")
	}

	// Regression guard 2: splitting the session by model *before* running
	// the ideal-cache algorithm (an earlier, broken version of
	// totalsByAgent did exactly this) drops step 2's carried-forward
	// context at the model switch, changing the ideal side.
	fragmentedIdeal := compute.SummarizeClaude(compute.ComputeIdealClaude([]compute.StepData{steps[0]}, sonnet), sonnet).Ideal +
		compute.SummarizeClaude(compute.ComputeIdealClaude([]compute.StepData{steps[1]}, opus), opus).Ideal
	if wantIdeal == fragmentedIdeal {
		t.Fatalf("test fixture doesn't distinguish continuous ideal computation from per-model-fragmented computation")
	}
}

// Unpriced models must not be folded into the totals at zero cost or at
// some other model's rate — they're excluded and reported separately.
func TestTotalsByAgent_UnpricedModelExcludedAndReported(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "claude", "s1", []testStep{
		step("claude-opus-4-6-does-not-exist", 100000, 0, 20000),
		step("claude-opus-4-6-does-not-exist", 50000, 0, 10000),
	})

	rows, unpriced, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}

	claude := agentTotalRow(t, rows, "CLAUDE")
	if claude.Actual != 0 || claude.Ideal != 0 {
		t.Errorf("CLAUDE actual/ideal = %v/%v, want 0/0 (only session uses an unpriced model)", claude.Actual, claude.Ideal)
	}

	if len(unpriced) != 1 {
		t.Fatalf("unpriced = %+v, want exactly 1 entry", unpriced)
	}
	u := unpriced[0]
	if u.Model != "claude-opus-4-6-does-not-exist" {
		t.Errorf("unpriced model = %q, want %q", u.Model, "claude-opus-4-6-does-not-exist")
	}
	if u.Steps != 2 {
		t.Errorf("unpriced steps = %d, want 2", u.Steps)
	}
	wantTokens := 100000 + 20000 + 50000 + 10000
	if u.Tokens != wantTokens {
		t.Errorf("unpriced tokens = %d, want %d", u.Tokens, wantTokens)
	}
}

// A missing rate must not delete money the provider actually billed. When the
// agent logged a cost, that cost belongs in Paid even though no rate exists
// to build an Ideal from; Ideal mirrors it so the step reads as zero overpay
// instead of vanishing from the report. Dropping it made the Paid column
// disagree with OpenCode's own bill ($179.20 against a logged $187.04).
func TestTotalsByAgent_UnpricedButLoggedCostStillCounts(t *testing.T) {
	st := withTempStore(t)

	const logged = 3.25
	ingestTotalTestSession(t, st, "opencode", "s1", []testStep{
		loggedStep("model-with-no-price-at-all", 5000, 1000, 500, logged),
	})

	rows, unpriced, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}

	oc := agentTotalRow(t, rows, "OC")
	if oc.Actual != logged {
		t.Errorf("OC actual = %v, want %v (the logged cost, despite the missing rate)", oc.Actual, logged)
	}
	// Equal, not zero: an unpriced step must not manufacture overpay it has
	// no rate to justify, nor savings.
	if oc.Ideal != logged {
		t.Errorf("OC ideal = %v, want %v (mirrored from the logged cost)", oc.Ideal, logged)
	}

	// Still reported — counted is not the same as priced, and the warning is
	// the only place the pricing gap is visible.
	if len(unpriced) != 1 {
		t.Fatalf("unpriced = %+v, want exactly 1 entry", unpriced)
	}
	if u := unpriced[0]; u.LoggedCost != logged {
		t.Errorf("unpriced LoggedCost = %v, want %v", u.LoggedCost, logged)
	}
}

// Paid must come from what PI logged, not from a recomputation. PI records
// the provider's own billed figure per message, which already reflects rate
// changes and discounts the local table cannot know about.
func TestTotalsByAgent_PIPrefersLoggedCost(t *testing.T) {
	st := withTempStore(t)

	prevPrices := piPricesFunc
	fake := map[string]compute.ModelPrices{
		"test-pi-model": {Input: 42, Output: 84, CacheRead: 4.2},
	}
	piPricesFunc = func() map[string]compute.ModelPrices { return fake }
	t.Cleanup(func() { piPricesFunc = prevPrices })

	// A cost nowhere near what the fake rates would produce, so a fallback
	// to recomputation shows up as a mismatch rather than a rounding error.
	const logged = 0.4242
	ingestTotalTestSession(t, st, "pi", "s1", []testStep{
		loggedStep("test-pi-model", 5000, 1000, 500, logged),
	})

	rows, _, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}

	pi := agentTotalRow(t, rows, "PI")
	if pi.Actual != logged {
		t.Errorf("PI actual = %v, want %v (the logged cost)", pi.Actual, logged)
	}

	recomputed := compute.PiStepActualCost(
		compute.StepData{Input: 5000, CacheRead: 1000, Output: 500}, fake["test-pi-model"])
	if recomputed == logged {
		t.Fatal("test fixture doesn't distinguish the logged cost from a recomputation")
	}
	// Ideal has nothing logged to read from, so it must still come from the
	// rate table — otherwise the overpay comparison has no basis.
	if pi.Ideal <= 0 {
		t.Errorf("PI ideal = %v, want > 0 (computed from rates)", pi.Ideal)
	}
}

// Steps the agent logged no cost for fall back to the rate table, so a
// partially-annotated session is summed from both sources.
func TestTotalsByAgent_FallsBackToRatesWhenNoLoggedCost(t *testing.T) {
	st := withTempStore(t)

	prevPrices := piPricesFunc
	fake := map[string]compute.ModelPrices{
		"test-pi-model": {Input: 42, Output: 84, CacheRead: 4.2},
	}
	piPricesFunc = func() map[string]compute.ModelPrices { return fake }
	t.Cleanup(func() { piPricesFunc = prevPrices })

	const logged = 0.4242
	ingestTotalTestSession(t, st, "pi", "s1", []testStep{
		loggedStep("test-pi-model", 5000, 1000, 500, logged),
		step("test-pi-model", 3000, 2000, 400), // no logged cost
	})

	rows, _, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}

	want := logged + compute.PiStepActualCost(
		compute.StepData{Input: 3000, CacheRead: 2000, Output: 400}, fake["test-pi-model"])
	pi := agentTotalRow(t, rows, "PI")
	if pi.Actual != want {
		t.Errorf("PI actual = %v, want %v (logged step + recomputed step)", pi.Actual, want)
	}
}

// Claude Code no longer writes costUSD, so message.cost for the claude agent
// is tokeneks' own ingest-time figure. Preferring it would pin the report to
// whatever rates were in effect at ingest and make `prices update` a silent
// no-op, so the claude row must always recompute.
func TestTotalsByAgent_ClaudeIgnoresStoredCost(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "claude", "s1", []testStep{
		loggedStep("claude-sonnet-5", 10000, 0, 2000, 999.99),
	})

	rows, _, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}

	claude := agentTotalRow(t, rows, "CLAUDE")
	if claude.Actual >= 999.99 {
		t.Errorf("CLAUDE actual = %v, want the recomputed cost, not the stored 999.99", claude.Actual)
	}
	want := compute.PiStepActualCost(
		compute.StepData{Input: 10000, Output: 2000}, claudeGlobalModelPrices()["claude-sonnet-5"])
	if claude.Actual != want {
		t.Errorf("CLAUDE actual = %v, want %v", claude.Actual, want)
	}
}

// The unpriced warning reports the money, not just the tokens, whenever the
// agent logged a cost — that is the whole point of the exclusion being
// visible.
func TestTotalsByAgent_UnpricedModelReportsLoggedCost(t *testing.T) {
	st := withTempStore(t)

	prevPrices := piPricesFunc
	piPricesFunc = func() map[string]compute.ModelPrices { return map[string]compute.ModelPrices{} }
	t.Cleanup(func() { piPricesFunc = prevPrices })

	ingestTotalTestSession(t, st, "pi", "s1", []testStep{
		loggedStep("retired-model", 5000, 1000, 500, 1.25),
		loggedStep("retired-model", 2000, 500, 100, 0.75),
	})

	rows, unpriced, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}
	// Summed across both steps, and still surfaced in the warning: being
	// counted is not the same as being priced.
	if got := agentTotalRow(t, rows, "PI").Actual; got != 2.0 {
		t.Errorf("PI actual = %v, want 2.0 (both logged costs, despite the missing rate)", got)
	}
	if len(unpriced) != 1 {
		t.Fatalf("unpriced = %+v, want 1 entry", unpriced)
	}
	if got := unpriced[0].LoggedCost; got != 2.0 {
		t.Errorf("unpriced LoggedCost = %v, want 2.0", got)
	}
	if unpriced[0].Agent != "PI" {
		t.Errorf("unpriced Agent = %q, want PI", unpriced[0].Agent)
	}
}
