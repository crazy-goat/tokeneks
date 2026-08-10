package main

import (
	"context"
	"testing"
	"time"
	"tokeneks/compute"
	"tokeneks/store"
)

// ingestSessionAt is ingestTotalTestSession (total_test.go) plus explicit
// control over LastActivity/CreatedAt. The date-window tests below need to
// pin sessions at exact millisecond boundaries around a [fromMs, toMs)
// range, which ingestTotalTestSession (always time.Now()) can't do.
func ingestSessionAt(t *testing.T, st *store.Store, agent, sessionID string, lastActivity int64, steps []testStep) {
	t.Helper()
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
			CreatedAt:    lastActivity,
		}}
	}
	ps := store.ParsedSession{
		Session:  store.Session{Agent: agent, SessionID: sessionID, CreatedAt: lastActivity, LastActivity: lastActivity},
		Messages: msgs,
	}
	if err := st.IngestSession(context.Background(), ps); err != nil {
		t.Fatalf("IngestSession(%s/%s): %v", agent, sessionID, err)
	}
}

// --- Issue 2: per-model rows must sum to the session's Paid -----------------

// A Claude session's message.cost column is tokeneks' own ingest-time
// recomputation, not a bill — it can go stale relative to today's rates.
// perModelUsage must reprice through computeSessionPricing (the same engine
// that produces the session-level Paid shown above it), not echo that
// column, or the per-model rows and the session total can disagree. The
// stored Cost here (999 per step) is deliberately absurd so a
// regression that goes back to summing the raw column would be caught
// immediately rather than by a subtle rounding difference.
func TestPerModelUsage_ClaudeRowsSumToSessionPaid(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "claude", "s1", []testStep{
		{Model: "claude-sonnet-5", Step: compute.StepData{Input: 20000, Output: 1000}, Cost: 999},
		{Model: "claude-opus-5", Step: compute.StepData{Input: 20000, Output: 1000}, Cost: 999},
	})

	sessions, err := gatherWebSessionsFromStore(context.Background(), time.Now().Add(-30*24*time.Hour).UnixMilli(), unboundedToMs)
	if err != nil {
		t.Fatalf("gatherWebSessionsFromStore: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	ws := sessions[0]
	if len(ws.Models) != 2 {
		t.Fatalf("models = %d, want 2: %+v", len(ws.Models), ws.Models)
	}

	var sum float64
	for _, m := range ws.Models {
		sum += m.Cost
	}
	if !approxEqual(sum, ws.TotalCost, 1e-9) {
		t.Errorf("sum(models.cost) = %v, want ws.TotalCost (Paid) = %v", sum, ws.TotalCost)
	}
	// The stale logged column would have summed to 999+999 = 1998; a real
	// recompute at today's per-token rates for 20k input / 1k output tokens
	// is nowhere close to that.
	if ws.TotalCost >= 100 {
		t.Errorf("ws.TotalCost = %v looks like it trusted the stale logged cost (999+999), not a recompute", ws.TotalCost)
	}
}

// Same invariant, checked against OpenCode, where message.cost IS the
// provider's real bill (CostIsLogged = true) rather than a recomputation —
// this was already correct before the fix, kept as a regression guard so a
// future change to perModelUsage can't quietly break the case that used to
// be the easy one.
func TestPerModelUsage_OpenCodeRowsSumToSessionPaidAcrossModels(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "opencode", "s1", []testStep{
		loggedStep("Kimi K2.6", 5000, 1000, 500, 1.23),
		loggedStep("gpt-5-codex", 3000, 500, 200, 0.55),
	})

	sessions, err := gatherWebSessionsFromStore(context.Background(), time.Now().Add(-30*24*time.Hour).UnixMilli(), unboundedToMs)
	if err != nil {
		t.Fatalf("gatherWebSessionsFromStore: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	ws := sessions[0]
	if len(ws.Models) != 2 {
		t.Fatalf("models = %d, want 2: %+v", len(ws.Models), ws.Models)
	}

	var sum float64
	for _, m := range ws.Models {
		sum += m.Cost
	}
	if !approxEqual(sum, ws.TotalCost, 1e-9) {
		t.Errorf("sum(models.cost) = %v, want ws.TotalCost = %v", sum, ws.TotalCost)
	}
	if !approxEqual(ws.TotalCost, 1.78, 1e-9) {
		t.Errorf("ws.TotalCost = %v, want 1.78 (1.23 + 0.55 logged)", ws.TotalCost)
	}
}

// A step whose model has no resolvable rate and no logged cost carries no
// cost information at all and must not be priced at some invented rate —
// per computeSessionPricing/unpricedModel's rule it is excluded from Paid
// entirely. The per-model rows must reflect exactly that same rule (Cost 0
// for that model) rather than a different one, or the rows and the session
// total disagree on what "unpriced" means.
func TestPerModelUsage_UnpricedModelExcludedFromBothTotalsConsistently(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "opencode", "s1", []testStep{
		loggedStep("Kimi K2.6", 5000, 1000, 500, 1.0),
		step("fake-model-no-price-xyz", 2000, 0, 100), // no rate, no logged cost
	})

	sessions, err := gatherWebSessionsFromStore(context.Background(), time.Now().Add(-30*24*time.Hour).UnixMilli(), unboundedToMs)
	if err != nil {
		t.Fatalf("gatherWebSessionsFromStore: %v", err)
	}
	ws := sessions[0]

	var sum float64
	var unpricedRowCost float64
	for _, m := range ws.Models {
		sum += m.Cost
		if m.Model == "fake-model-no-price-xyz" {
			unpricedRowCost = m.Cost
		}
	}
	if !approxEqual(sum, ws.TotalCost, 1e-9) {
		t.Errorf("sum(models.cost) = %v, want ws.TotalCost = %v", sum, ws.TotalCost)
	}
	if unpricedRowCost != 0 {
		t.Errorf("unpriced model's row Cost = %v, want 0", unpricedRowCost)
	}
	if ws.TotalCost != 1.0 {
		t.Errorf("ws.TotalCost = %v, want 1.0 (only the priced/logged step)", ws.TotalCost)
	}
}

// --- Issue 1: the session window is an explicit [fromMs, toMs) range -------

// gatherWebSessionsFromStore must filter on session.last_activity, which is
// stored in MILLISECONDS, with fromMs inclusive and toMs exclusive — get
// either the units or the boundary wrong and this either matches everything
// or silently drops sessions right at the edges of a picked date.
func TestGatherWebSessionsFromStore_WindowIsMillisecondExclusiveOnLastActivity(t *testing.T) {
	st := withTempStore(t)

	const fromMs int64 = 1_700_000_000_000
	const toMs int64 = 1_700_000_100_000

	one := []testStep{step("claude-sonnet-5", 100, 0, 10)}
	ingestSessionAt(t, st, "claude", "before", fromMs-1, one)
	ingestSessionAt(t, st, "claude", "at-from", fromMs, one)
	ingestSessionAt(t, st, "claude", "inside", fromMs+50_000, one)
	ingestSessionAt(t, st, "claude", "at-to", toMs, one)
	ingestSessionAt(t, st, "claude", "after", toMs+1, one)

	sessions, err := gatherWebSessionsFromStore(context.Background(), fromMs, toMs)
	if err != nil {
		t.Fatalf("gatherWebSessionsFromStore: %v", err)
	}

	got := map[string]bool{}
	for _, s := range sessions {
		got[s.ID] = true
	}
	for _, want := range []string{"at-from", "inside"} {
		if !got[want] {
			t.Errorf("missing session %q from [%d, %d)", want, fromMs, toMs)
		}
	}
	for _, unwanted := range []string{"before", "at-to", "after"} {
		if got[unwanted] {
			t.Errorf("session %q should not be in [%d, %d)", unwanted, fromMs, toMs)
		}
	}
}

// TestAggregateSessionsFromStore_DateFilterUsesLocalNotUTC is a regression
// test for the bug this fix removes: the SQL behind aggregateSessionsFromStore
// took date(last_activity, 'unixepoch') — which SQLite computes in UTC by
// default — and compared it straight against date, a local calendar day the
// caller typed. A session with last_activity near local midnight landed on
// the wrong side of the filter whenever the machine's UTC offset was
// nonzero, the same class of bug fixed in claudeSessions (claude.go) and
// dashboardWindowMs (web.go). aggregateSessionsFromStore's date branch has
// no wired-up CLI flag today (every call site passes ""), but the bug lives
// in the function regardless of whether anything currently reaches it.
func TestAggregateSessionsFromStore_DateFilterUsesLocalNotUTC(t *testing.T) {
	instant, localDate, ok := localVsUTCDayMismatch(t)
	if !ok {
		t.Log("time.Local == UTC on this machine; cannot exercise the local-vs-UTC distinction here")
		return
	}

	st := withTempStore(t)
	ingestSessionAt(t, st, "claude", "sess-1", instant.UnixMilli(), []testStep{
		step("claude-sonnet-5", 100, 0, 10),
	})

	utcDate := instant.UTC().Format("2006-01-02")
	if utcDate == localDate {
		t.Fatalf("test setup bug: utcDate (%s) should differ from localDate (%s)", utcDate, localDate)
	}

	got, err := aggregateSessionsFromStore(context.Background(), "claude", 0, localDate)
	if err != nil {
		t.Fatalf("aggregateSessionsFromStore: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("aggregateSessionsFromStore(date=%s) returned %d sessions, want 1 (an old UTC-based date() would have filed this session under %s instead)", localDate, len(got), utcDate)
	}

	// The instant's UTC calendar day must NOT match — proving the filter is
	// anchored on local, not coincidentally matching both.
	gotUTC, err := aggregateSessionsFromStore(context.Background(), "claude", 0, utcDate)
	if err != nil {
		t.Fatalf("aggregateSessionsFromStore: %v", err)
	}
	if len(gotUTC) != 0 {
		t.Errorf("aggregateSessionsFromStore(date=%s) returned %d sessions, want 0 (that's the instant's UTC day, not its local one)", utcDate, len(gotUTC))
	}
}

// A dashboard request for a calendar range and the CLI's own rolling
// --days N window (aggregateSessionsFromStore's cutoff, mirrored here by
// calling gatherWebSessionsFromStore with the equivalent fromMs/toMs
// dashboardWindowMs derives for "no start/end given") must agree on Paid —
// this is the same invariant TestGatherWebSessionsFromStore_AgreesWithTotalsByAgent
// checks for the days-based path; this one drives it through
// dashboardWindowMs specifically, so a regression in that function (e.g.
// reintroducing a slack fudge factor) is caught even if the two "days"
// call sites never see each other again after this fix.
func TestGatherWebSessionsFromStore_MatchesRollingCLIWindow(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "opencode", "s1", []testStep{
		loggedStep("Kimi K2.6", 5000, 1000, 500, 1.23),
	})

	fromMs, toMs := dashboardWindowMs(30, "", "")
	viaDashboard, err := gatherWebSessionsFromStore(context.Background(), fromMs, toMs)
	if err != nil {
		t.Fatalf("gatherWebSessionsFromStore: %v", err)
	}

	rows, _, err := totalsByAgent(context.Background(), 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}
	oc := agentTotalRow(t, rows, "OC")

	var dashboardTotal float64
	for _, s := range viaDashboard {
		if s.Agent == "OpenCode" {
			dashboardTotal += s.TotalCost
		}
	}
	if !approxEqual(dashboardTotal, oc.Actual, 1e-9) {
		t.Errorf("dashboard (no start/end) OC total = %v, CLI --days 30 OC total = %v", dashboardTotal, oc.Actual)
	}
}

// TestBuildPricingSpecs_ClaudePricesAtMessageTime pins the one thing that
// distinguishes a historical report from a wrong one: a message is priced at
// the rate that was in effect when it was sent.
//
// The claude spec used to take an "at" it never read, hoisting a single
// resolved-at-now price map out of the closure. That was invisible for as
// long as no built-in Claude model had a dated window in the past — and
// claude-sonnet-5's is still in the future today, so no real session can
// expose it either. It would have started overstating every pre-cutover
// Sonnet 5 session by 50% on the day the window opened, with nothing in the
// output to say the numbers had moved.
func TestBuildPricingSpecs_ClaudePricesAtMessageTime(t *testing.T) {
	if _, ok := claudeJSONOverlayPrices()["claude-sonnet-5"]; ok {
		t.Skip("~/.tokeneks/claude_models.json overrides claude-sonnet-5; the built-in windows this test pins are not in play")
	}

	spec := buildPricingSpecs()["claude"]

	pre, ok := spec.PriceFunc("claude-sonnet-5", claudeSonnet5PriceChange.Add(-24*time.Hour).UnixMilli())
	if !ok {
		t.Fatal("claude-sonnet-5 unpriced before the cutover")
	}
	post, ok := spec.PriceFunc("claude-sonnet-5", claudeSonnet5PriceChange.Add(24*time.Hour).UnixMilli())
	if !ok {
		t.Fatal("claude-sonnet-5 unpriced after the cutover")
	}

	if pre.Output == post.Output {
		t.Fatalf("PriceFunc ignores its timestamp: a message from before %s prices at the same output rate (%v) as one from after",
			claudeSonnet5PriceChange.Format("2006-01-02"), pre.Output)
	}
	if pre.Output != 10.0 {
		t.Errorf("pre-cutover output rate = %v, want 10.0 (introductory)", pre.Output)
	}
	if post.Output != 15.0 {
		t.Errorf("post-cutover output rate = %v, want 15.0", post.Output)
	}
}
