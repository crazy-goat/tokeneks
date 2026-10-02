package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"tokeneks/compute"
)

// ocFixtureRow is one row to seed into the fixture "part" table. created
// only needs to establish an ORDER BY time_created sequence, not a real
// timestamp. messageID defaults to "msg-"+id when empty (most tests don't
// care which message a part belongs to); model is written into that
// message's synthetic data.modelID so ocSteps/ocStepsBatch's join can read
// it back — see the model field's fixture setup below for why this needs a
// second table now.
type ocFixtureRow struct {
	id        string
	sessionID string
	created   int64
	data      string // raw JSON for the part's data column
	messageID string // which message this part belongs to; default "msg-"+id
	model     string // model that served this step, stored on the message row
}

// withOCFixtureDB builds a minimal sqlite db shaped like OpenCode's own
// (just the part/message columns ocSteps/ocStepsBatch actually select), then
// swaps it in as the package-level OC connection for the duration of the
// test. ocSteps and ocStepsBatch always go through openOCDB()'s memoized
// singleton (see helpers.go) with no way to inject a path, so a fixture can
// only reach them by taking the singleton's place directly — the same
// trick TestOpenOCDB_ReturnsSameInstance and web_test.go's ocDB reset use.
//
// The message table exists here because that's where OpenCode actually
// stores the model that served a step (confirmed against the live db —
// message.data.modelID, falling back to message.data.model.modelID — see
// ocSteps' query and its doc comment on ocStep). A part-only fixture can no
// longer stand in for the real schema now that ocSteps joins across it.
func withOCFixtureDB(t *testing.T, rows []ocFixtureRow) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}

	// time_updated is NOT NULL in OpenCode's real schema but unused by the
	// queries under test, so it gets a placeholder value.
	if _, err := db.Exec(`CREATE TABLE part (
		id TEXT PRIMARY KEY,
		message_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL,
		time_updated INTEGER NOT NULL,
		data TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create part table: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE message (
		id TEXT PRIMARY KEY,
		data TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create message table: %v", err)
	}

	for _, r := range rows {
		messageID := r.messageID
		if messageID == "" {
			messageID = "msg-" + r.id
		}
		// INSERT OR IGNORE: several part rows in one test (e.g. a
		// step-finish plus a tool part) can share the same message, and
		// the message row only needs to exist once.
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO message (id, data) VALUES (?, ?)`,
			messageID, fmt.Sprintf(`{"modelID":%q}`, r.model),
		); err != nil {
			t.Fatalf("insert fixture message %s: %v", messageID, err)
		}
		if _, err := db.Exec(
			`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
			r.id, messageID, r.sessionID, r.created, r.created, r.data,
		); err != nil {
			t.Fatalf("insert fixture row %s: %v", r.id, err)
		}
	}

	ocDBMu.Lock()
	prevDB, prevErr := ocDB, ocDBErr
	ocDB, ocDBErr = db, nil
	ocDBMu.Unlock()

	t.Cleanup(func() {
		ocDBMu.Lock()
		_ = db.Close()
		ocDB, ocDBErr = prevDB, prevErr
		ocDBMu.Unlock()
	})
}

// TestOcSteps_CacheWritePopulatesCacheCreation is the core regression test
// for the bug: ocSteps used to select only $.tokens.input/.cache.read/
// .output, so $.tokens.cache.write never made it into StepData.CacheCreation
// and every OC step looked like it never wrote to cache.
func TestOcSteps_CacheWritePopulatesCacheCreation(t *testing.T) {
	withOCFixtureDB(t, []ocFixtureRow{
		{
			id:        "p1",
			sessionID: "sess-cw",
			created:   1,
			data:      `{"type":"step-finish","tokens":{"input":10,"output":5,"cache":{"read":100,"write":50000}}}`,
		},
	})

	steps, err := ocSteps("sess-cw")
	if err != nil {
		t.Fatalf("ocSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("got %d steps, want 1", len(steps))
	}
	s := steps[0].Data
	if s.CacheCreation != 50000 {
		t.Errorf("CacheCreation = %d, want 50000", s.CacheCreation)
	}
	if s.Input != 10 || s.CacheRead != 100 || s.Output != 5 {
		t.Errorf("other fields regressed: %+v", s)
	}
	// OpenCode's JSON has no TTL split (just tokens.cache.read/write) — see
	// the field-name confirmation query against the real db. There is
	// nothing to carry into CacheCreation1h, so it must stay at its zero
	// value rather than being (wrongly) inferred from CacheCreation.
	if s.CacheCreation1h != 0 {
		t.Errorf("CacheCreation1h = %d, want 0 (OpenCode has no TTL split)", s.CacheCreation1h)
	}
}

// TestOcSteps_NoCacheWriteKeyYieldsZero covers a step-finish row whose
// tokens.cache object has no "write" key. Real OpenCode rows always carry
// it (confirmed against the live db — zero NULLs for
// $.tokens.cache.write across every step-finish row), but a fixture or a
// future OpenCode version isn't guaranteed to. json_extract returns SQL
// NULL for an absent path, and database/sql refuses to Scan a NULL into a
// plain *int, so the query wraps it in ifnull(...,0) — without that, this
// case would be a scan error, not a zero.
func TestOcSteps_NoCacheWriteKeyYieldsZero(t *testing.T) {
	withOCFixtureDB(t, []ocFixtureRow{
		{
			id:        "p1",
			sessionID: "sess-no-write-key",
			created:   1,
			data:      `{"type":"step-finish","tokens":{"input":3,"output":4,"cache":{"read":7}}}`,
		},
	})

	steps, err := ocSteps("sess-no-write-key")
	if err != nil {
		t.Fatalf("ocSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("got %d steps, want 1", len(steps))
	}
	if steps[0].Data.CacheCreation != 0 {
		t.Errorf("CacheCreation = %d, want 0", steps[0].Data.CacheCreation)
	}
	if steps[0].Data.CacheRead != 7 {
		t.Errorf("CacheRead = %d, want 7 (unaffected by the missing write key)", steps[0].Data.CacheRead)
	}
}

// TestOcSteps_IgnoresNonStepFinishRows guards the existing WHERE
// json_extract(data,'$.type')='step-finish' filter — a fixture row of a
// different type (e.g. "tool") must not leak into the result just because
// it happens to carry a tokens.cache.write field.
func TestOcSteps_IgnoresNonStepFinishRows(t *testing.T) {
	withOCFixtureDB(t, []ocFixtureRow{
		{id: "p1", sessionID: "sess-mixed", created: 1, data: `{"type":"tool","tokens":{"input":1,"output":1,"cache":{"write":999}}}`},
		{id: "p2", sessionID: "sess-mixed", created: 2, data: `{"type":"step-finish","tokens":{"input":1,"output":1,"cache":{"read":0,"write":42}}}`},
	})

	steps, err := ocSteps("sess-mixed")
	if err != nil {
		t.Fatalf("ocSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("got %d steps, want 1 (the tool row must be filtered out)", len(steps))
	}
	if steps[0].Data.CacheCreation != 42 {
		t.Errorf("CacheCreation = %d, want 42", steps[0].Data.CacheCreation)
	}
}

// TestOcSteps_OcStepsBatch_Agree is the drift guard the task calls out
// explicitly: ocSteps and ocStepsBatch are two independent queries over the
// same shape (one keyed by a single session_id, the other by session_id IN
// (...)), and the cache-write bug already lived in both in lockstep — a fix
// applied to only one would silently reintroduce exactly this kind of
// single/batch divergence.
func TestOcSteps_OcStepsBatch_Agree(t *testing.T) {
	rows := []ocFixtureRow{
		{id: "p1", sessionID: "sess-a", created: 1, data: `{"type":"step-finish","cost":1.23,"tokens":{"input":10,"output":1,"cache":{"read":5,"write":100}}}`, model: "model-a1"},
		{id: "p2", sessionID: "sess-a", created: 2, data: `{"type":"step-finish","tokens":{"input":20,"output":2,"cache":{"read":15,"write":0}}}`, model: "model-a2"}, // no cost key
		{id: "p3", sessionID: "sess-b", created: 1, data: `{"type":"step-finish","tokens":{"input":30,"output":3,"cache":{"read":0}}}`, model: "model-b1"},            // no write key
		{id: "p4", sessionID: "sess-b", created: 2, data: `{"type":"step-finish","tokens":{"input":40,"output":4,"cache":{"read":0,"write":7000}}}`, model: "model-b2"},
	}
	withOCFixtureDB(t, rows)

	ids := []string{"sess-a", "sess-b"}
	batch, err := ocStepsBatch(ids)
	if err != nil {
		t.Fatalf("ocStepsBatch: %v", err)
	}

	for _, id := range ids {
		single, err := ocSteps(id)
		if err != nil {
			t.Fatalf("ocSteps(%s): %v", id, err)
		}
		batched := batch[id]
		if len(single) != len(batched) {
			t.Fatalf("%s: ocSteps returned %d steps, ocStepsBatch returned %d", id, len(single), len(batched))
		}
		for i := range single {
			if single[i] != batched[i] {
				t.Errorf("%s step %d: ocSteps=%+v ocStepsBatch=%+v diverge", id, i, single[i], batched[i])
			}
		}
	}

	// Spot-check the actual values so a bug that happens to affect both
	// queries identically (e.g. both still reading the wrong JSON path)
	// wouldn't slip through just because they still agree with each other.
	if got := batch["sess-a"][0].Data.CacheCreation; got != 100 {
		t.Errorf("sess-a step 0 CacheCreation = %d, want 100", got)
	}
	if got := batch["sess-b"][0].Data.CacheCreation; got != 0 {
		t.Errorf("sess-b step 0 (no write key) CacheCreation = %d, want 0", got)
	}
	if got := batch["sess-b"][1].Data.CacheCreation; got != 7000 {
		t.Errorf("sess-b step 1 CacheCreation = %d, want 7000", got)
	}
	// LoggedCost is the field this task adds to the query — same drift risk
	// as CacheCreation above, so it gets the same single-vs-batch and
	// present-vs-absent spot checks.
	if got := batch["sess-a"][0].LoggedCost; got != 1.23 {
		t.Errorf("sess-a step 0 LoggedCost = %v, want 1.23", got)
	}
	if got := batch["sess-a"][1].LoggedCost; got != 0 {
		t.Errorf("sess-a step 1 (no cost key) LoggedCost = %v, want 0", got)
	}
	// Model is the field this change adds to the query (joined in from the
	// message table, not read off the part row itself) — same drift risk as
	// CacheCreation/LoggedCost above.
	if got := batch["sess-a"][0].Model; got != "model-a1" {
		t.Errorf("sess-a step 0 Model = %q, want %q", got, "model-a1")
	}
	if got := batch["sess-b"][1].Model; got != "model-b2" {
		t.Errorf("sess-b step 1 Model = %q, want %q", got, "model-b2")
	}
}

// TestOcSteps_ModelComesFromMessageTable is the core regression test for the
// per-step model bug: OpenCode's step-finish part carries no model at all,
// so the model has to be joined in from the message the part belongs to.
// This session mimics the real 27%-of-steps case that motivated the whole
// change — two steps in one session, each served by a different model —
// and checks each step keeps its own model rather than both collapsing to
// one.
func TestOcSteps_ModelComesFromMessageTable(t *testing.T) {
	withOCFixtureDB(t, []ocFixtureRow{
		{id: "p1", sessionID: "sess-multi", created: 1, messageID: "msg-1", model: "gpt-oss-120b",
			data: `{"type":"step-finish","tokens":{"input":10,"output":1,"cache":{"read":0,"write":0}}}`},
		{id: "p2", sessionID: "sess-multi", created: 2, messageID: "msg-2", model: "deepseek-v4",
			data: `{"type":"step-finish","tokens":{"input":20,"output":2,"cache":{"read":0,"write":0}}}`},
	})

	steps, err := ocSteps("sess-multi")
	if err != nil {
		t.Fatalf("ocSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(steps))
	}
	if steps[0].Model != "gpt-oss-120b" {
		t.Errorf("step 0 Model = %q, want %q", steps[0].Model, "gpt-oss-120b")
	}
	if steps[1].Model != "deepseek-v4" {
		t.Errorf("step 1 Model = %q, want %q", steps[1].Model, "deepseek-v4")
	}
}

// TestOcSteps_NoMatchingMessageYieldsEmptyModel covers a part row whose
// message_id has no corresponding message row (shouldn't happen in the real
// db — confirmed zero orphans there — but the query uses a LEFT JOIN
// specifically so a step never silently vanishes from the result just
// because its message went missing; it should come back with an empty
// model instead, which resolveAgentPrices then correctly treats as
// unpriced rather than dropping the row's tokens from the count entirely).
func TestOcSteps_NoMatchingMessageYieldsEmptyModel(t *testing.T) {
	withOCFixtureDB(t, nil) // creates the schema with no rows yet

	db, err := openOCDB()
	if err != nil {
		t.Fatalf("openOCDB: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('p1', 'no-such-message', 'sess-orphan', 1, 1, ?)`,
		`{"type":"step-finish","tokens":{"input":5,"output":1,"cache":{"read":0,"write":0}}}`,
	); err != nil {
		t.Fatalf("insert orphan part: %v", err)
	}

	steps, err := ocSteps("sess-orphan")
	if err != nil {
		t.Fatalf("ocSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("got %d steps, want 1 (the row must not be dropped just because its message is missing)", len(steps))
	}
	if steps[0].Model != "" {
		t.Errorf("Model = %q, want empty string", steps[0].Model)
	}
	if steps[0].Data.Input != 5 {
		t.Errorf("Input = %d, want 5 (token data must survive even without a resolvable model)", steps[0].Data.Input)
	}
}

// ocTestPrices stands in for whatever ocModelPrices/the store would resolve
// a model to. Used directly (bypassing resolveAgentPrices) in the
// ocActualCost tests below so they exercise only the preference logic, not
// the price-resolution chain — that chain has its own tests in
// prices_resolve_test.go.
var ocTestPrices = compute.ModelPrices{Input: 0.95, CacheRead: 0.16, Output: 4.00}

// TestOcActualCost_AllStepsLogged_UsesLoggedSum is the first case the task
// calls out: a session whose steps all logged a cost must report that sum,
// not the rate table's figure for the same tokens. The two are deliberately
// ~40x apart (logged $80 vs. ~$1.90 the table would compute for 2M input
// tokens at ocTestPrices.Input) so a bug that silently falls back to the
// table shows up as a large, obvious failure rather than a rounding
// difference that could be mistaken for noise.
func TestOcActualCost_AllStepsLogged_UsesLoggedSum(t *testing.T) {
	steps := []ocStep{
		{Data: compute.StepData{Input: 1_000_000}, LoggedCost: 50},
		{Data: compute.StepData{Input: 1_000_000}, LoggedCost: 30},
	}

	got := ocActualCost(steps, ocTestPrices)
	if got != 80 {
		t.Errorf("ocActualCost() = %v, want 80 (the logged sum)", got)
	}

	rateTableTotal := compute.PiStepActualCost(steps[0].Data, ocTestPrices) + compute.PiStepActualCost(steps[1].Data, ocTestPrices)
	if got == rateTableTotal {
		t.Fatalf("test setup produced identical logged and rate-table totals (%v); it can no longer catch a wrongful fallback", got)
	}
}

// TestOcActualCost_NoLoggedCost_FallsBackToRateTable is the second case: a
// step that logged nothing (LoggedCost == 0) must be priced from the rate
// table, exactly as totalsByAgent does for PI and OC.
func TestOcActualCost_NoLoggedCost_FallsBackToRateTable(t *testing.T) {
	step := ocStep{Data: compute.StepData{Input: 1_000_000, Output: 200_000, CacheRead: 50_000}, LoggedCost: 0}

	got := ocActualCost([]ocStep{step}, ocTestPrices)
	want := compute.PiStepActualCost(step.Data, ocTestPrices)
	if want == 0 {
		t.Fatal("test setup produced a zero expected cost; it wouldn't distinguish a working fallback from a silently-broken one")
	}
	if got != want {
		t.Errorf("ocActualCost() = %v, want %v (the rate-table figure)", got, want)
	}
}

// TestOcActualCost_MixedSession_SumsBothSources is the third case: a session
// with one logged step and one unlogged step must sum the logged figure for
// the first and the rate-table figure for the second, not pick one source
// for the whole session.
func TestOcActualCost_MixedSession_SumsBothSources(t *testing.T) {
	logged := ocStep{Data: compute.StepData{Input: 1_000_000}, LoggedCost: 10}
	unlogged := ocStep{Data: compute.StepData{Input: 2_000_000, Output: 100_000}, LoggedCost: 0}

	got := ocActualCost([]ocStep{logged, unlogged}, ocTestPrices)
	want := logged.LoggedCost + compute.PiStepActualCost(unlogged.Data, ocTestPrices)
	if got != want {
		t.Errorf("ocActualCost() = %v, want %v (logged + rate-table, blended per step)", got, want)
	}
}

// TestOcSessionSummary_ActualIsLoggedIdealStaysRateTable wires
// ocActualCost's preference into ocSessionSummary (which also has to run it
// through resolveAgentPrices) and checks that only Actual moves: Ideal is a
// counterfactual with nothing logged to read, so it must stay exactly what
// compute.Summarize computes from the rate table, both before and after this
// change.
func TestOcSessionSummary_ActualIsLoggedIdealStaysRateTable(t *testing.T) {
	withTempStore(t) // isolates resolveAgentPrices from the real ~/.local/share/tokeneks store

	const model = "Kimi K2.6" // priced by the ocModelPrices builtin fallback in algo.go
	steps := []ocStep{
		{Data: compute.StepData{Input: 1_000_000, CacheRead: 200_000}, LoggedCost: 50, Model: model},
	}

	summary, unpriced, ok := ocSessionSummary(steps)
	if !ok {
		t.Fatal("expected Kimi K2.6 to resolve via the builtin price table")
	}
	if len(unpriced) != 0 {
		t.Fatalf("unexpected unpriced entries: %+v", unpriced)
	}
	if summary.Actual != 50 {
		t.Errorf("Actual = %v, want 50 (logged), not the rate table's ~$%.2f", summary.Actual,
			compute.PiStepActualCost(steps[0].Data, ocTestPrices))
	}

	tokenSteps := []compute.StepData{steps[0].Data}
	wantIdeal := compute.Summarize(compute.ComputeIdeal(tokenSteps), ocTestPrices).Ideal
	if summary.Ideal != wantIdeal {
		t.Errorf("Ideal = %v, want %v (unaffected by the logged-cost preference)", summary.Ideal, wantIdeal)
	}

	wantOverpay := max(summary.Actual-summary.Ideal, 0)
	if summary.Overpay != wantOverpay {
		t.Errorf("Overpay = %v, want %v (recomputed from the corrected Actual)", summary.Overpay, wantOverpay)
	}
}

// TestOcSessionSummary_TwoModelSession_PricedPerModel is the direct
// regression test for the bug this task fixes: a session whose steps are
// served by two different models must price each step at its own model's
// rate, not the whole session at one. Kimi K2.6 and GPT 5.4 mini (both in
// algo.go's ocModelPrices builtin table) have deliberately different rates
// (Input 0.95 vs 0.75, Output 4.00 vs 4.50) so a fixture that accidentally
// collapsed to one model's rate would produce a visibly wrong number, not a
// rounding difference.
func TestOcSessionSummary_TwoModelSession_PricedPerModel(t *testing.T) {
	withTempStore(t)

	kimi := ocModelPrices["Kimi K2.6"]
	gpt := ocModelPrices["GPT 5.4 mini"]

	tokenSteps := []compute.StepData{
		{Input: 1_000_000, Output: 100_000},                     // served by Kimi K2.6
		{Input: 1_000_000, CacheRead: 200_000, Output: 100_000}, // served by GPT 5.4 mini
	}
	steps := []ocStep{
		{Data: tokenSteps[0], Model: "Kimi K2.6"},
		{Data: tokenSteps[1], Model: "GPT 5.4 mini"},
	}

	summary, unpriced, ok := ocSessionSummary(steps)
	if !ok {
		t.Fatal("expected both models to resolve via the builtin price table")
	}
	if len(unpriced) != 0 {
		t.Fatalf("unexpected unpriced entries: %+v", unpriced)
	}

	// Correct: one continuous ComputeIdeal pass over both steps (so step 1's
	// ideal cache read still credits step 0's carried-forward output), then
	// each resulting row priced at its own model's rate — the same
	// construction totalsByAgent uses in main.go for the identical problem.
	idealRows := compute.ComputeIdeal(tokenSteps)
	wantActual := compute.PiStepActualCost(compute.StepData{Input: idealRows[0].Input, CacheRead: idealRows[0].CacheRead, Output: idealRows[0].Output}, kimi) +
		compute.PiStepActualCost(compute.StepData{Input: idealRows[1].Input, CacheRead: idealRows[1].CacheRead, Output: idealRows[1].Output}, gpt)
	wantIdeal := compute.PiStepActualCost(compute.StepData{Input: idealRows[0].IdealIn, CacheRead: idealRows[0].IdealCR, Output: idealRows[0].Output}, kimi) +
		compute.PiStepActualCost(compute.StepData{Input: idealRows[1].IdealIn, CacheRead: idealRows[1].IdealCR, Output: idealRows[1].Output}, gpt)

	if summary.Actual != wantActual {
		t.Errorf("Actual = %v, want %v (per-row pricing on the continuous session)", summary.Actual, wantActual)
	}
	if summary.Ideal != wantIdeal {
		t.Errorf("Ideal = %v, want %v (continuous ideal computation, priced per row)", summary.Ideal, wantIdeal)
	}

	// Regression guard 1: the old bug priced the whole session at the first
	// step's model instead of per-step.
	allAtKimi := compute.Summarize(compute.ComputeIdeal(tokenSteps), kimi)
	if summary.Actual == allAtKimi.Actual {
		t.Fatalf("test fixture doesn't distinguish per-model pricing from whole-session-at-first-model pricing")
	}

	// Regression guard 2: splitting the session by model *before* running
	// the ideal-cache algorithm (which an earlier, broken version of this
	// exact function did) drops step 1's carried-forward context at the
	// model switch, changing the ideal side. This is the case
	// TestTotalsByAgent_SessionSpanningTwoModelsPricedPerModel in
	// total_test.go guards for PI/Claude; this is the same guard for OC.
	fragmentedIdeal := compute.Summarize(compute.ComputeIdeal([]compute.StepData{tokenSteps[0]}), kimi).Ideal +
		compute.Summarize(compute.ComputeIdeal([]compute.StepData{tokenSteps[1]}), gpt).Ideal
	if wantIdeal == fragmentedIdeal {
		t.Fatalf("test fixture doesn't distinguish continuous ideal computation from per-model-fragmented computation")
	}
}

// TestOcSessionSummary_MixedPricedAndUnpriced_AttributesPerStep is the
// unpriced-attribution contract: a session mixing a priced and an unpriced
// model must report only the unpriced model's own tokens and logged cost in
// the unpriced bucket, and must still price the rest of the session
// normally — not drop the whole session just because one step in it has no
// price.
func TestOcSessionSummary_MixedPricedAndUnpriced_AttributesPerStep(t *testing.T) {
	withTempStore(t)

	priced := ocStep{
		Data:       compute.StepData{Input: 1_000_000, Output: 100_000},
		Model:      "Kimi K2.6",
		LoggedCost: 12.34,
	}
	unpriced := ocStep{
		Data:       compute.StepData{Input: 500_000, CacheRead: 10_000, Output: 20_000},
		Model:      "Totally Unknown Model XYZ",
		LoggedCost: 7.77,
	}

	summary, unpricedList, ok := ocSessionSummary([]ocStep{priced, unpriced})
	if !ok {
		t.Fatal("expected the priced step to keep the session in the totals")
	}
	if summary.Actual != priced.LoggedCost {
		t.Errorf("Actual = %v, want %v (only the priced step's logged cost)", summary.Actual, priced.LoggedCost)
	}
	wantTokens := unpriced.Data.Input + unpriced.Data.CacheRead + unpriced.Data.Output
	if summary.TotalIn+summary.TotalCR+summary.TotalOut != priced.Data.Input+priced.Data.Output {
		t.Errorf("priced totals leaked the unpriced step's tokens: TotalIn=%d TotalCR=%d TotalOut=%d",
			summary.TotalIn, summary.TotalCR, summary.TotalOut)
	}

	if len(unpricedList) != 1 {
		t.Fatalf("unpriced = %+v, want exactly 1 entry", unpricedList)
	}
	u := unpricedList[0]
	if u.Model != unpriced.Model {
		t.Errorf("unpriced model = %q, want %q", u.Model, unpriced.Model)
	}
	if u.Steps != 1 {
		t.Errorf("unpriced steps = %d, want 1", u.Steps)
	}
	if u.Tokens != wantTokens {
		t.Errorf("unpriced tokens = %d, want %d (only the unpriced step's own tokens)", u.Tokens, wantTokens)
	}
	if u.LoggedCost != unpriced.LoggedCost {
		t.Errorf("unpriced logged cost = %v, want %v", u.LoggedCost, unpriced.LoggedCost)
	}
}

// TestOcSessionSummary_LoggedCostPreferencePerModel checks that the
// logged-cost preference and per-model pricing compose correctly: a step
// with a logged cost uses it regardless of model, and a step without one
// falls back to *that step's own* model rate, not some other step's.
func TestOcSessionSummary_LoggedCostPreferencePerModel(t *testing.T) {
	withTempStore(t)

	gpt := ocModelPrices["GPT 5.4 mini"]
	logged := ocStep{Data: compute.StepData{Input: 1_000_000}, Model: "Kimi K2.6", LoggedCost: 3.5}
	unlogged := ocStep{Data: compute.StepData{Input: 1_000_000, Output: 50_000}, Model: "GPT 5.4 mini", LoggedCost: 0}

	summary, unpriced, ok := ocSessionSummary([]ocStep{logged, unlogged})
	if !ok {
		t.Fatal("expected both models to resolve")
	}
	if len(unpriced) != 0 {
		t.Fatalf("unexpected unpriced entries: %+v", unpriced)
	}

	want := logged.LoggedCost + compute.PiStepActualCost(unlogged.Data, gpt)
	if summary.Actual != want {
		t.Errorf("Actual = %v, want %v (logged cost for step 0 + GPT-rate recompute for step 1)", summary.Actual, want)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. ocList/ocDetail print with fmt.Printf directly
// rather than through an injectable io.Writer, so this is the only way to
// assert on their output from a test.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return buf.String()
}

// withOCFixtureSessionDB builds a fixture with the session/project/part/
// message shape ocSessions and ocList's query need (withOCFixtureDB above
// only has "part"+"message", which is enough for ocSteps/ocStepsBatch but
// not for a session list). The session/project/part shape mirrors
// TestOCSessions_UsesStoredStepFinishCost in web_test.go, which established
// that as the minimal shape those queries need; message is added here for
// the same reason it was added to withOCFixtureDB — ocStepsBatch now joins
// across it to reach each step's model.
func withOCFixtureSessionDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}

	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT, model TEXT, time_created INTEGER, tokens_input INTEGER, tokens_output INTEGER, tokens_cache_read INTEGER, tokens_cache_write INTEGER, parent_id TEXT, cost REAL, project_id TEXT);`,
		`CREATE TABLE project (id TEXT PRIMARY KEY, name TEXT, worktree TEXT);`,
		`CREATE TABLE part (session_id TEXT, time_created INTEGER, message_id TEXT, data TEXT);`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, data TEXT);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	ocDBMu.Lock()
	prevDB, prevErr := ocDB, ocDBErr
	ocDB, ocDBErr = db, nil
	ocDBMu.Unlock()

	t.Cleanup(func() {
		ocDBMu.Lock()
		_ = db.Close()
		ocDB, ocDBErr = prevDB, prevErr
		ocDBMu.Unlock()
	})

	return db
}

// TestOcSessions_DateFilterUsesLocalNotUTC is a regression test for the bug
// this fix removes: the SQL behind ocSessions took date(s.time_created,
// 'unixepoch') — which SQLite computes in UTC by default — and compared it
// straight against --date, a local calendar day the user typed. A session
// created near local midnight landed on the wrong side of the filter
// whenever the machine's UTC offset was nonzero, the same class of bug
// fixed in claudeSessions (claude.go) and dashboardWindowMs (web.go).
func TestOcSessions_DateFilterUsesLocalNotUTC(t *testing.T) {
	instant, localDate, ok := localVsUTCDayMismatch(t)
	if !ok {
		t.Log("time.Local == UTC on this machine; cannot exercise the local-vs-UTC distinction here")
		return
	}

	db := withOCFixtureSessionDB(t)

	createdAt := instant.UnixMilli()
	stmts := []string{
		fmt.Sprintf(`INSERT INTO session (id, title, model, time_created, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, parent_id, cost, project_id) VALUES ('sess-1', 'T', '{"id":"Kimi K2.6","providerID":"kimi"}', %d, 0, 0, 0, 0, '', 0, '');`, createdAt),
		`INSERT INTO message (id, data) VALUES ('msg-1', '{"modelID":"Kimi K2.6"}');`,
		fmt.Sprintf(`INSERT INTO part (session_id, time_created, message_id, data) VALUES ('sess-1', %d, 'msg-1', '{"type":"step-finish","cost":1,"tokens":{"input":1000,"output":0,"cache":{"read":0,"write":0}}}');`, createdAt),
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	utcDate := instant.UTC().Format("2006-01-02")
	if utcDate == localDate {
		t.Fatalf("test setup bug: utcDate (%s) should differ from localDate (%s)", utcDate, localDate)
	}

	got, err := ocSessions(3650, localDate)
	if err != nil {
		t.Fatalf("ocSessions() = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ocSessions(date=%s) returned %d sessions, want 1 (an old UTC-based date() would have filed this session under %s instead)", localDate, len(got), utcDate)
	}

	// The instant's UTC calendar day must NOT match — proving the filter is
	// anchored on local, not coincidentally matching both.
	gotUTC, err := ocSessions(3650, utcDate)
	if err != nil {
		t.Fatalf("ocSessions() = %v", err)
	}
	if len(gotUTC) != 0 {
		t.Errorf("ocSessions(date=%s) returned %d sessions, want 0 (that's the instant's UTC day, not its local one)", utcDate, len(gotUTC))
	}
}

// TestOcList_UnpricedModelExcludedFromTotalsButReportedWithLoggedCost is the
// contract the task explicitly protects: a model with no price still has no
// Ideal, so — even though its Paid is now readable straight off the logged
// cost — it must stay out of both the Actual and Ideal columns and only
// surface in the unpriced-models warning block, the same as before this
// change. Folding it into Paid alone would make the Paid and Ideal columns
// cover two different sets of sessions, which is the exact inconsistency
// this task removes elsewhere.
func TestOcList_UnpricedModelExcludedFromTotalsButReportedWithLoggedCost(t *testing.T) {
	withTempStore(t)
	db := withOCFixtureSessionDB(t)

	now := time.Now().UnixMilli()
	stmts := []string{
		fmt.Sprintf(`INSERT INTO session (id, title, model, time_created, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, parent_id, cost, project_id) VALUES ('sess-priced', 'Priced', '{"id":"Kimi K2.6","providerID":"kimi"}', %d, 0, 0, 0, 0, '', 0, '');`, now-4000),
		`INSERT INTO message (id, data) VALUES ('msg-priced', '{"modelID":"Kimi K2.6"}');`,
		fmt.Sprintf(`INSERT INTO part (session_id, time_created, message_id, data) VALUES ('sess-priced', %d, 'msg-priced', '{"type":"step-finish","cost":50,"tokens":{"input":1000000,"output":0,"cache":{"read":0,"write":0}}}');`, now-3900),

		fmt.Sprintf(`INSERT INTO session (id, title, model, time_created, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, parent_id, cost, project_id) VALUES ('sess-unpriced', 'Unpriced', '{"id":"Totally Unknown Model XYZ","providerID":"whoever"}', %d, 0, 0, 0, 0, '', 0, '');`, now-2000),
		`INSERT INTO message (id, data) VALUES ('msg-unpriced', '{"modelID":"Totally Unknown Model XYZ"}');`,
		fmt.Sprintf(`INSERT INTO part (session_id, time_created, message_id, data) VALUES ('sess-unpriced', %d, 'msg-unpriced', '{"type":"step-finish","cost":7.77,"tokens":{"input":500000,"output":0,"cache":{"read":0,"write":0}}}');`, now-1900),
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	out := captureStdout(t, func() {
		if err := ocList(1, ""); err != nil {
			t.Fatalf("ocList: %v", err)
		}
	})

	if !strings.Contains(out, "sess-priced") {
		t.Errorf("priced session's row is missing from the output:\n%s", out)
	}
	if strings.Contains(out, "sess-unpriced") {
		t.Errorf("unpriced session leaked a row into the priced table (should only appear in the warning block):\n%s", out)
	}
	if !strings.Contains(out, "Totally Unknown Model XYZ") {
		t.Errorf("unpriced model missing from the warning block:\n%s", out)
	}
	if !strings.Contains(out, "7.77") {
		t.Errorf("unpriced model's logged cost missing from the warning block:\n%s", out)
	}
	// TOTAL Paid must equal the priced session's logged $50 — not its
	// rate-table figure (1,000,000 input tokens * $0.95/M = $0.95) — and not
	// inflated by the unpriced session's $7.77, which has no Ideal to sit
	// next to and so must stay out of both columns. Overpay ($49.05 =
	// 50.00-0.95) is the clearest single-number witness that Paid moved to
	// the logged figure while Ideal stayed on the rate table.
	if !strings.Contains(out, "50.00") {
		t.Errorf("TOTAL Paid doesn't show the priced session's logged $50:\n%s", out)
	}
	if !strings.Contains(out, "49.05") {
		t.Errorf("TOTAL Overpay doesn't reflect logged Paid ($50) minus rate-table Ideal ($0.95):\n%s", out)
	}
}

// TestOcList_SessionSpanningTwoModels_PricedPerModel is the end-to-end
// version of TestOcSessionSummary_TwoModelSession_PricedPerModel: a single
// OpenCode session whose two steps were served by different models (the
// session's own s.model column only ever reflects one of them — here, the
// first) must still price each step at its own rate through the full
// ocList path, not collapse to the session-level "DominantModel" the way
// the pre-fix code did.
func TestOcList_SessionSpanningTwoModels_PricedPerModel(t *testing.T) {
	withTempStore(t)
	db := withOCFixtureSessionDB(t)

	now := time.Now().UnixMilli()
	stmts := []string{
		// s.model deliberately names only the first step's model — mirrors
		// OpenCode's real session.model column, which is set once and never
		// reflects a later mid-session switch.
		fmt.Sprintf(`INSERT INTO session (id, title, model, time_created, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, parent_id, cost, project_id) VALUES ('sess-two-models', 'Two models', '{"id":"Kimi K2.6","providerID":"kimi"}', %d, 0, 0, 0, 0, '', 0, '');`, now-4000),
		`INSERT INTO message (id, data) VALUES ('msg-1', '{"modelID":"Kimi K2.6"}');`,
		`INSERT INTO message (id, data) VALUES ('msg-2', '{"modelID":"GPT 5.4 mini"}');`,
		fmt.Sprintf(`INSERT INTO part (session_id, time_created, message_id, data) VALUES ('sess-two-models', %d, 'msg-1', '{"type":"step-finish","tokens":{"input":1000000,"output":100000,"cache":{"read":0,"write":0}}}');`, now-3900),
		fmt.Sprintf(`INSERT INTO part (session_id, time_created, message_id, data) VALUES ('sess-two-models', %d, 'msg-2', '{"type":"step-finish","tokens":{"input":1000000,"output":100000,"cache":{"read":200000,"write":0}}}');`, now-3800),
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	out := captureStdout(t, func() {
		if err := ocList(1, ""); err != nil {
			t.Fatalf("ocList: %v", err)
		}
	})

	if strings.Contains(out, "no price") {
		t.Errorf("unexpected unpriced warning; both models are in the builtin table:\n%s", out)
	}

	kimi := ocModelPrices["Kimi K2.6"]
	gpt := ocModelPrices["GPT 5.4 mini"]
	tokenSteps := []compute.StepData{
		{Input: 1_000_000, Output: 100_000},
		{Input: 1_000_000, CacheRead: 200_000, Output: 100_000},
	}
	idealRows := compute.ComputeIdeal(tokenSteps)
	wantActual := compute.PiStepActualCost(compute.StepData{Input: idealRows[0].Input, CacheRead: idealRows[0].CacheRead, Output: idealRows[0].Output}, kimi) +
		compute.PiStepActualCost(compute.StepData{Input: idealRows[1].Input, CacheRead: idealRows[1].CacheRead, Output: idealRows[1].Output}, gpt)

	// A row/TOTAL priced wholly at Kimi's rate (the pre-fix behavior, since
	// s.model = "Kimi K2.6") would print a visibly different Paid figure;
	// assert the correct per-model figure appears instead.
	wantStr := fmt.Sprintf("%.2f", wantActual)
	if !strings.Contains(out, wantStr) {
		t.Errorf("output doesn't contain the per-model-priced Actual %s:\n%s", wantStr, out)
	}

	allAtKimi := compute.Summarize(compute.ComputeIdeal(tokenSteps), kimi)
	wrongStr := fmt.Sprintf("%.2f", allAtKimi.Actual)
	if wantStr == wrongStr {
		t.Fatalf("test fixture doesn't distinguish per-model pricing from whole-session-at-Kimi pricing")
	}
	if strings.Contains(out, wrongStr) {
		t.Errorf("output contains the whole-session-at-Kimi figure %s, want only the per-model figure %s:\n%s", wrongStr, wantStr, out)
	}
}

// TestOcDetail_PrintsReconciliation_WhenLoggedDiffersMeaningfullyFromRate is
// the end-to-end version of the algo_test.go computeDetailCostRecon unit
// tests: a real oc detail run on a session whose logged cost is far from
// what tokeneks' price table would compute for the same tokens must print
// the "Price-table drift" block after the table, naming both totals — this
// is the exact divergence the task describes (table sums to the rate,
// headline sums the logged figure) made visible instead of silent.
func TestOcDetail_PrintsReconciliation_WhenLoggedDiffersMeaningfullyFromRate(t *testing.T) {
	withTempStore(t)
	db := withOCFixtureSessionDB(t)

	now := time.Now().UnixMilli()
	kimi := ocModelPrices["Kimi K2.6"]
	rate := compute.PiStepActualCost(compute.StepData{Input: 1_000_000, Output: 100_000}, kimi)
	logged := rate * 2 // deliberately far from the rate so this can't be mistaken for rounding

	stmts := []string{
		fmt.Sprintf(`INSERT INTO session (id, title, model, time_created, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, parent_id, cost, project_id) VALUES ('sess-recon', 'Recon', '{"id":"Kimi K2.6","providerID":"kimi"}', %d, 0, 0, 0, 0, '', 0, '');`, now),
		`INSERT INTO message (id, data) VALUES ('msg-recon', '{"modelID":"Kimi K2.6"}');`,
		fmt.Sprintf(`INSERT INTO part (session_id, time_created, message_id, data) VALUES ('sess-recon', %d, 'msg-recon', '{"type":"step-finish","cost":%f,"tokens":{"input":1000000,"output":100000,"cache":{"read":0,"write":0}}}');`, now+1, logged),
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	out := captureStdout(t, func() {
		if err := ocDetail("sess-recon"); err != nil {
			t.Fatalf("ocDetail: %v", err)
		}
	})

	if !strings.Contains(out, "Price-table drift") {
		t.Fatalf("output missing the reconciliation block for a session whose logged cost is 2x the rate-derived cost:\n%s", out)
	}
	if !strings.Contains(out, "1/1 priced rows carried a logged cost") {
		t.Errorf("output missing the covered-rows fraction:\n%s", out)
	}
	rateStr := fmt.Sprintf("%.4f", rate)
	loggedStr := fmt.Sprintf("%.4f", logged)
	if !strings.Contains(out, rateStr) {
		t.Errorf("output missing the rate-derived total %s:\n%s", rateStr, out)
	}
	if !strings.Contains(out, loggedStr) {
		t.Errorf("output missing the logged provider total %s:\n%s", loggedStr, out)
	}
	// Actual paid (the headline) must show the logged figure, exactly the
	// number this reconciliation block explains the table's absence of.
	if !strings.Contains(out, fmt.Sprintf("Actual paid:  $%.2f", logged)) {
		t.Errorf("headline doesn't show the logged Actual paid figure:\n%s", out)
	}
}

// TestOcDetail_NoReconciliation_WhenNothingLogged covers the "no noise"
// contract: a session where every step-finish part carries a zero/absent
// cost has nothing to reconcile the rate-derived table against, so the
// block must not appear at all.
func TestOcDetail_NoReconciliation_WhenNothingLogged(t *testing.T) {
	withTempStore(t)
	db := withOCFixtureSessionDB(t)

	now := time.Now().UnixMilli()
	stmts := []string{
		fmt.Sprintf(`INSERT INTO session (id, title, model, time_created, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, parent_id, cost, project_id) VALUES ('sess-nolog', 'NoLog', '{"id":"Kimi K2.6","providerID":"kimi"}', %d, 0, 0, 0, 0, '', 0, '');`, now),
		`INSERT INTO message (id, data) VALUES ('msg-nolog', '{"modelID":"Kimi K2.6"}');`,
		fmt.Sprintf(`INSERT INTO part (session_id, time_created, message_id, data) VALUES ('sess-nolog', %d, 'msg-nolog', '{"type":"step-finish","tokens":{"input":1000000,"output":100000,"cache":{"read":0,"write":0}}}');`, now+1),
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	out := captureStdout(t, func() {
		if err := ocDetail("sess-nolog"); err != nil {
			t.Fatalf("ocDetail: %v", err)
		}
	})

	if strings.Contains(out, "Price-table drift") {
		t.Errorf("output contains a reconciliation block for a session with nothing logged:\n%s", out)
	}
}
