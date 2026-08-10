package main

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"tokeneks/compute"
	"tokeneks/store"
)

// claudeCarryForwardLine builds one raw assistant-message JSONL line with
// explicit cache fields, for scenarios claudeLine (input/output only) can't
// express.
func claudeCarryForwardLine(id, model string, input, cacheRead, cacheCreation, output int) string {
	return `{"type":"assistant","message":{"id":"` + id + `","model":"` + model +
		`","usage":{"input_tokens":` + strconv.Itoa(input) +
		`,"cache_read_input_tokens":` + strconv.Itoa(cacheRead) +
		`,"cache_creation_input_tokens":` + strconv.Itoa(cacheCreation) +
		`,"output_tokens":` + strconv.Itoa(output) +
		`},"content":[{"type":"text"}]},"timestamp":"2026-08-03T16:00:00.000Z"}`
}

// TestClaudeDetail_CarryForwardAcrossModelSwitch is a regression test for
// the bug this fix exists for: claudeDetail used to run the ideal-cache pass
// once per model (groupStepsByModel), which discards the carried-forward
// prompt-cache state at every model switch. Here the whole first turn's
// context (Input+Output) is read back verbatim under a *different* model on
// the very next turn — a perfect cache hit spanning the switch. Splitting by
// model makes that second turn look like a cold start, so its ideal cost is
// computed off the full input rate instead of the far cheaper cache-read
// rate, inflating Ideal past Actual for a session that is a full cache hit
// throughout and should report zero overpay.
func TestClaudeDetail_CarryForwardAcrossModelSwitch(t *testing.T) {
	withTempStore(t)

	path := writeClaudeJSONL(t, []string{
		// Turn 1, claude-opus-5: 1M fresh input tokens, no cache activity yet.
		claudeCarryForwardLine("msg1", "claude-opus-5", 1_000_000, 0, 0, 100_000),
		// Turn 2, a *different* model: reads back exactly turn 1's context
		// (1M input + 100k output = 1.1M) from cache. A correct continuous
		// ideal-cache pass recognizes this as a full hit; a pass restarted
		// per model sees 1.1M tokens of cache_read with no prior state and
		// prices them as if they were fresh input.
		claudeCarryForwardLine("msg2", "claude-haiku-4-5", 0, 1_100_000, 0, 50_000),
	})

	var err error
	out := captureStdout(t, func() { err = claudeDetail(path) })
	if err != nil {
		t.Fatalf("claudeDetail() = %v\noutput:\n%s", err, out)
	}

	actual, ideal := parseTotalActualIdeal(t, out)

	// claude-opus-5: Input=5.0/M, Output=25.0/M. claude-haiku-4-5:
	// CacheRead=0.1/M, Output=5.0/M (see claudeBuiltinPriceWindows).
	wantActual := (1_000_000*5.0+100_000*25.0)/1e6 + (1_100_000*0.1+50_000*5.0)/1e6
	const centTolerance = 0.006
	if math.Abs(actual-wantActual) > centTolerance {
		t.Errorf("Actual paid = $%.6f, want $%.6f", actual, wantActual)
	}
	// Ideal, computed correctly (continuous pass): turn 2 is a full cache
	// hit, so its ideal cost equals its actual cost — same total as Actual.
	// The old, split-by-model computation priced turn 2's 1.1M ideal tokens
	// as fresh Input (at $1.0/M) instead of CacheRead (at $0.1/M), which
	// would print Ideal = $8.85 here instead of $7.86 — MORE than Actual,
	// the impossible result this test guards against.
	if math.Abs(ideal-wantActual) > centTolerance {
		t.Errorf("Ideal paid = $%.6f, want $%.6f (a session that is a full cache hit throughout should have Ideal == Actual)", ideal, wantActual)
	}
	if actual < ideal-centTolerance {
		t.Errorf("Actual ($%.6f) < Ideal ($%.6f) — the impossible split-by-model result this fix exists to prevent", actual, ideal)
	}
}

// TestClaudeList_CarryForwardAcrossModelSwitch is the same regression as
// TestClaudeDetail_CarryForwardAcrossModelSwitch but through claudeList's
// aggregate path (the second of the two Claude call sites that used to
// split by model).
func TestClaudeList_CarryForwardAcrossModelSwitch(t *testing.T) {
	withTempStore(t)

	baseDir := t.TempDir()
	projectDir := filepath.Join(baseDir, "proj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(projectDir, "session1.jsonl")
	content := claudeCarryForwardLine("msg1", "claude-opus-5", 1_000_000, 0, 0, 100_000) + "\n" +
		claudeCarryForwardLine("msg2", "claude-haiku-4-5", 0, 1_100_000, 0, 50_000) + "\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	prevBase := defaultClaudeSessions
	defaultClaudeSessions = baseDir
	t.Cleanup(func() { defaultClaudeSessions = prevBase })

	var err error
	out := captureStdout(t, func() { err = claudeList(3650, "") })
	if err != nil {
		t.Fatalf("claudeList() = %v\noutput:\n%s", err, out)
	}

	// Columns: TOTAL, Tokens, Paid, Ideal, Overpay, %ideal, $/1M, i$/1M —
	// Paid is field[2], Ideal is field[3].
	wantActual := (1_000_000*5.0+100_000*25.0)/1e6 + (1_100_000*0.1+50_000*5.0)/1e6
	paid, ideal := parseListTotalRow(t, out, 2, 3)
	const centTolerance = 0.006
	if math.Abs(paid-wantActual) > centTolerance {
		t.Errorf("TOTAL Paid = $%.6f, want $%.6f", paid, wantActual)
	}
	if math.Abs(ideal-wantActual) > centTolerance {
		t.Errorf("TOTAL Ideal = $%.6f, want $%.6f (continuous carry-forward across the model switch, not the inflated split-by-model figure)", ideal, wantActual)
	}
	if paid < ideal-centTolerance {
		t.Errorf("TOTAL Paid ($%.6f) < Ideal ($%.6f) — the impossible split-by-model result this fix exists to prevent", paid, ideal)
	}
}

// TestClaudeSessions_DateFilterUsesLocalNotUTC is a regression test for the
// bug this fix removes: claudeSessions compared --date (a local calendar
// day the user typed) against activity.UTC().Format("2006-01-02"), so a
// session near local midnight fell on the wrong side of the filter whenever
// the machine's UTC offset was nonzero — the same class of bug already
// fixed server-side for the web dashboard by dashboardWindowMs (web.go).
func TestClaudeSessions_DateFilterUsesLocalNotUTC(t *testing.T) {
	instant, localDate, ok := localVsUTCDayMismatch(t)
	if !ok {
		t.Log("time.Local == UTC on this machine; cannot exercise the local-vs-UTC distinction here")
		return
	}

	baseDir := t.TempDir()
	projectDir := filepath.Join(baseDir, "proj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(projectDir, "session1.jsonl")
	line := `{"type":"assistant","message":{"id":"msg1","model":"claude-opus-5","usage":{"input_tokens":100,"output_tokens":50},"content":[{"type":"text"}]},"timestamp":"` +
		instant.UTC().Format(time.RFC3339Nano) + `"}` + "\n"
	if err := os.WriteFile(fp, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	prevBase := defaultClaudeSessions
	defaultClaudeSessions = baseDir
	t.Cleanup(func() { defaultClaudeSessions = prevBase })

	utcDate := instant.UTC().Format("2006-01-02")
	if utcDate == localDate {
		t.Fatalf("test setup bug: utcDate (%s) should differ from localDate (%s)", utcDate, localDate)
	}

	got, err := claudeSessions(3650, localDate, "")
	if err != nil {
		t.Fatalf("claudeSessions() = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claudeSessions(date=%s) returned %d sessions, want 1 (an old UTC-based comparison would have filed this session under %s instead)", localDate, len(got), utcDate)
	}
	if got[0].Date != localDate {
		t.Errorf("session.Date = %q, want %q (the local calendar day, not the UTC one)", got[0].Date, localDate)
	}

	// The instant's UTC calendar day must NOT match — proving the filter is
	// anchored on local, not coincidentally matching both.
	gotUTC, err := claudeSessions(3650, utcDate, "")
	if err != nil {
		t.Fatalf("claudeSessions() = %v", err)
	}
	if len(gotUTC) != 0 {
		t.Errorf("claudeSessions(date=%s) returned %d sessions, want 0 (that's the instant's UTC day, not its local one)", utcDate, len(gotUTC))
	}
}

// writeClaudeJSONL writes lines (already-serialized JSON, one per line) to a
// fresh file under t.TempDir() and returns its path.
func writeClaudeJSONL(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// claudeLine builds one assistant-message JSONL line. usage is repeated
// verbatim across every content-block line of a real message, per the bug
// claudeMessages works around — tests that want to exercise dedup pass the
// same id/usage to several calls.
func claudeLine(id, model string, input, output int, content string) string {
	return `{"type":"assistant","message":{"id":"` + id + `","model":"` + model +
		`","usage":{"input_tokens":` + strconv.Itoa(input) + `,"output_tokens":` + strconv.Itoa(output) +
		`},"content":` + content + `},"timestamp":"2026-08-03T16:00:00.000Z"}`
}

func TestClaudeMessages_MultiBlockSameID_CountsUsageOnce(t *testing.T) {
	// Claude Code writes one line per content block; a thinking block, a
	// text block, and a tool call from the same assistant turn all repeat
	// the identical message.usage. That must collapse into one step.
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"thinking"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"tool_use"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(res.Steps))
	}
	want := compute.StepData{Input: 100, Output: 50}
	if res.Steps[0].Step != want {
		t.Errorf("Steps[0].Step = %+v, want %+v (usage must be counted once, not per block)", res.Steps[0].Step, want)
	}
	if len(res.Models) != 1 {
		t.Errorf("Models = %v, want exactly one entry (one per message, not per block)", res.Models)
	}
}

func TestClaudeMessages_ToolCallsSurviveDedup(t *testing.T) {
	// Three tool_use blocks spread across three lines of one message must
	// still add up to three tool calls, even though the lines collapse into
	// a single step.
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"tool_use"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"tool_use"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"tool_use"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(res.Steps))
	}
	if res.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3", res.ToolCalls)
	}
}

func TestClaudeMessages_DistinctIDsStayDistinctSteps(t *testing.T) {
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
		claudeLine("msg2", "claude-sonnet-5", 200, 75, `[{"type":"text"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2", len(res.Steps))
	}
	if len(res.Models) != 2 {
		t.Errorf("Models = %v, want 2 entries", res.Models)
	}
}

func TestClaudeMessages_EmptyIDsNotMergedTogether(t *testing.T) {
	// An empty message.id can't be used as a merge key, so unlike a shared
	// real id, two id-less lines must never collapse into one step — that
	// would conflate two unrelated messages. Each stays its own step.
	path := writeClaudeJSONL(t, []string{
		claudeLine("", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
		claudeLine("", "claude-sonnet-5", 200, 75, `[{"type":"text"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2 (id-less lines must not merge with each other)", len(res.Steps))
	}
}

func TestClaudeMessages_ZeroUsageLineDoesNotCreatePhantomOrZeroRealUsage(t *testing.T) {
	// A defensive case: a zero-usage line sharing an id with a real line
	// must not spawn an extra ("phantom") step, and must not zero out the
	// real usage either. This isn't known to happen in real session files
	// (usage is identical across every block of one message there), but the
	// dedup logic must not depend on that to stay correct.
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 0, 0, `[{"type":"thinking"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1 (zero-usage line must not create a phantom step)", len(res.Steps))
	}
	want := compute.StepData{Input: 100, Output: 50}
	if res.Steps[0].Step != want {
		t.Errorf("Steps[0].Step = %+v, want %+v (zero-usage line must not zero out the real one)", res.Steps[0].Step, want)
	}
}

// TestResolveClaudeSessionPath_FileFound covers the unchanged happy path:
// resolveClaudeSessionPath must still resolve a bare session id to its file
// when the file is actually on disk.
func TestResolveClaudeSessionPath_FileFound(t *testing.T) {
	baseDir := t.TempDir()
	projectDir := filepath.Join(baseDir, "proj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(projectDir, "abc123.jsonl")
	if err := os.WriteFile(fp, []byte(claudeLine("m1", "claude-sonnet-5", 10, 5, `[{"type":"text"}]`)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevBase := defaultClaudeSessions
	defaultClaudeSessions = baseDir
	t.Cleanup(func() { defaultClaudeSessions = prevBase })

	gotFP, gotID, err := resolveClaudeSessionPath("abc123")
	if err != nil {
		t.Fatalf("resolveClaudeSessionPath() = %v", err)
	}
	if gotFP != fp {
		t.Errorf("resolveClaudeSessionPath() path = %q, want %q", gotFP, fp)
	}
	if gotID != "abc123" {
		t.Errorf("resolveClaudeSessionPath() id = %q, want %q", gotID, "abc123")
	}
}

// TestResolveClaudeSessionPath_NotFound_ReturnsSentinelType pins down the
// distinguishable "clean miss" error type claudeDetail relies on to decide
// whether to fall back to the store — a plain fmt.Errorf here would make
// that decision indistinguishable from a genuine walk failure.
func TestResolveClaudeSessionPath_NotFound_ReturnsSentinelType(t *testing.T) {
	baseDir := t.TempDir()
	prevBase := defaultClaudeSessions
	defaultClaudeSessions = baseDir
	t.Cleanup(func() { defaultClaudeSessions = prevBase })

	_, _, err := resolveClaudeSessionPath("nope")
	var notFound *claudeSessionNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("resolveClaudeSessionPath() error = %v (%T), want *claudeSessionNotFoundError", err, err)
	}
}

// ingestFakeClaudeSession writes a minimal claude session straight into the
// store, bypassing the file-based parser entirely — used to simulate "the
// source JSONL is gone but the session survived in the store" without ever
// touching real ingest code or real files.
func ingestFakeClaudeSession(t *testing.T, st *store.Store, sessionID, project string, msgs []store.Message) {
	t.Helper()
	ps := store.ParsedSession{
		Session: store.Session{
			Agent:        "claude",
			SessionID:    sessionID,
			Project:      project,
			CreatedAt:    msgs[0].CreatedAt,
			LastActivity: msgs[len(msgs)-1].CreatedAt,
		},
	}
	for i, m := range msgs {
		m.Agent = "claude"
		m.SessionID = sessionID
		m.MsgIndex = i
		m.Role = store.RoleAssistant
		ps.Messages = append(ps.Messages, store.ParsedMessage{Message: m})
	}
	if err := st.IngestSession(context.Background(), ps); err != nil {
		t.Fatalf("IngestSession: %v", err)
	}
}

// TestClaudeDetail_FallsBackToStoreWhenFileMissing is the core case this fix
// exists for: a session whose source JSONL has been rotated/deleted but is
// still cached in the local store (a real cache since commit 2486511) must
// still render, sourced from the store, with a visible note that the file
// is gone rather than silently pretending nothing changed.
func TestClaudeDetail_FallsBackToStoreWhenFileMissing(t *testing.T) {
	st := withTempStore(t)
	resetClaudeUnknownWarnings()

	emptyDir := t.TempDir()
	prevBase := defaultClaudeSessions
	defaultClaudeSessions = emptyDir
	t.Cleanup(func() { defaultClaudeSessions = prevBase })

	sessionID := "missing-session-1"
	now := time.Now().UnixMilli()
	// claude-haiku-4-5 has a single, dated-window-free built-in price
	// ($1.0/M in, $5.0/M out) so the expected dollar figure doesn't depend
	// on which side of claudeSonnet5PriceChange the test happens to run.
	ingestFakeClaudeSession(t, st, sessionID, "work/example", []store.Message{
		{Model: "claude-haiku-4-5", InputTokens: 1_000_000, OutputTokens: 500_000, CreatedAt: now},
	})

	var err error
	out := captureStdout(t, func() { err = claudeDetail(sessionID) })
	if err != nil {
		t.Fatalf("claudeDetail() = %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "File:     (not found on disk)") {
		t.Errorf("output missing the 'not found on disk' File line:\n%s", out)
	}
	if !strings.Contains(out, "note:") || !strings.Contains(out, "store cache") {
		t.Errorf("output missing a visible fallback note telling the user the source is gone:\n%s", out)
	}
	if !strings.Contains(out, "work/example") {
		t.Errorf("output missing the project recorded in the store:\n%s", out)
	}
	if !strings.Contains(out, "claude-haiku-4-5") {
		t.Errorf("output missing the model recorded in the store:\n%s", out)
	}

	actual, ideal := parseTotalActualIdeal(t, out)
	wantActual := (1_000_000*1.0 + 500_000*5.0) / 1e6
	const centTolerance = 0.006
	if math.Abs(actual-wantActual) > centTolerance {
		t.Errorf("Actual paid = $%.6f, want $%.6f", actual, wantActual)
	}
	// No cache activity at all in this session, so Ideal must equal Actual.
	if math.Abs(ideal-wantActual) > centTolerance {
		t.Errorf("Ideal paid = $%.6f, want $%.6f", ideal, wantActual)
	}
}

// TestClaudeDetail_NotFoundAnywhere_ReturnsNotFoundError covers the case
// where neither the file walk nor the store know the session: the original
// "session not found" message must still surface, not some new fallback
// error, and it must not silently succeed.
func TestClaudeDetail_NotFoundAnywhere_ReturnsNotFoundError(t *testing.T) {
	withTempStore(t)

	emptyDir := t.TempDir()
	prevBase := defaultClaudeSessions
	defaultClaudeSessions = emptyDir
	t.Cleanup(func() { defaultClaudeSessions = prevBase })

	err := claudeDetail("does-not-exist-anywhere")
	if err == nil {
		t.Fatal("claudeDetail() = nil, want a not-found error")
	}
	if !strings.Contains(err.Error(), "Claude session not found: does-not-exist-anywhere") {
		t.Errorf("claudeDetail() error = %v, want it to carry the original not-found message", err)
	}
}

// TestClaudeDetail_WalkFailureNotMisreportedAsNotFound guards against a real
// I/O error (here: the sessions root itself doesn't exist, which is what a
// permissions problem or a corrupt mount would also surface as) being
// swallowed into the friendlier-looking "session not found" / store-fallback
// path. Such an error must propagate as-is.
func TestClaudeDetail_WalkFailureNotMisreportedAsNotFound(t *testing.T) {
	withTempStore(t)

	prevBase := defaultClaudeSessions
	defaultClaudeSessions = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { defaultClaudeSessions = prevBase })

	err := claudeDetail("anything")
	if err == nil {
		t.Fatal("claudeDetail() = nil, want a walk error")
	}
	if strings.Contains(err.Error(), "session not found") {
		t.Errorf("claudeDetail() error = %v — a walk/stat failure must not be reported as \"session not found\"", err)
	}
}

func TestClaudeMessages_ZeroUsageLineFirst_RealLineStillCreatesStep(t *testing.T) {
	// Same as above but with the zero-usage line arriving first in the
	// file, to make sure the "first line for an id" isn't assumed to be the
	// one that determines the step's usage.
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
		claudeLine("msg1", "claude-sonnet-5", 0, 0, `[{"type":"thinking"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(res.Steps))
	}
	want := compute.StepData{Input: 100, Output: 50}
	if res.Steps[0].Step != want {
		t.Errorf("Steps[0].Step = %+v, want %+v", res.Steps[0].Step, want)
	}
}
