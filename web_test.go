package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"tokeneks/compute"
	"tokeneks/store"

	_ "github.com/mattn/go-sqlite3"
)

func TestValidatePort(t *testing.T) {
	// Every accepted and rejected row of the boundary audit of issue #94, so a
	// future change to validatePort has to make a deliberate decision.
	cases := []struct {
		name    string
		port    string
		wantErr bool
	}{
		{name: "lowest accepted port", port: "1"},
		{name: "typical port", port: "8080"},
		{name: "highest accepted port", port: "65535"},
		// Leading zeros stay accepted: "0080" is a plain decimal number in
		// range, and the printed URL http://localhost:0080 opens port 80.
		{name: "leading zeros", port: "0080"},
		{name: "empty", port: "", wantErr: true},
		{name: "zero", port: "0", wantErr: true},
		{name: "above range", port: "65536", wantErr: true},
		{name: "negative", port: "-1", wantErr: true},
		{name: "negative zero", port: "-0", wantErr: true},
		{name: "letters", port: "abc", wantErr: true},
		{name: "digits then letters", port: "80x", wantErr: true},
		{name: "decimal point", port: "8080.0", wantErr: true},
		{name: "hexadecimal notation", port: "0x50", wantErr: true},
		{name: "digit separator", port: "1_0", wantErr: true},
		{name: "leading space", port: " 8080", wantErr: true},
		{name: "trailing space", port: "8080 ", wantErr: true},
		{name: "trailing newline", port: "8080\n", wantErr: true},
		{name: "trailing tab", port: "8080\t", wantErr: true},
		// A leading plus is a typo that net.Listen accepts (":+8080" binds
		// port 8080) but that no client can open, so it is rejected (#94).
		{name: "leading plus", port: "+8080", wantErr: true},
		{name: "leading plus on the lowest accepted port", port: "+1", wantErr: true},
	}
	const wantMsg = "must be an integer between 1 and 65535"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePort(tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validatePort(%q) = nil, want error", tc.port)
				}
				if !strings.Contains(err.Error(), wantMsg) {
					t.Errorf("validatePort(%q) error = %q, want it to contain %q", tc.port, err, wantMsg)
				}
				return
			}
			if err != nil {
				t.Errorf("validatePort(%q) = %v, want nil", tc.port, err)
			}
		})
	}
}

func TestWebPagesUseEmbeddedChartJS(t *testing.T) {
	const script = `<script src="/static/chart.umd.min.js"></script>`
	for name, page := range map[string][]byte{"index": webIndexHTML, "detail": webDetailHTML} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(string(page), script) {
				t.Error("page must load the embedded Chart.js endpoint")
			}
			if strings.Contains(string(page), "cdn.jsdelivr.net") {
				t.Error("page must not depend on the Chart.js CDN")
			}
		})
	}
	if len(chartJS) == 0 {
		t.Error("embedded Chart.js asset is empty")
	}
}

func resetOCDBForTest(t *testing.T) {
	t.Helper()
	ocDBMu.Lock()
	defer ocDBMu.Unlock()
	if ocDB != nil {
		_ = ocDB.Close()
	}
	ocDB = nil
	ocDBErr = nil
}

func TestHandleAPISessionDetail_BadPath_Returns400(t *testing.T) {
	// /api/session/ with no agent/id should return 400, not panic
	req := httptest.NewRequest(http.MethodGet, "/api/session/", nil)
	w := httptest.NewRecorder()

	// Create a minimal mux to route the request
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/", handleAPISessionDetail)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
}

func TestHandleAPISessionStream_BadPath_Returns400(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/session-stream/", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/session-stream/", handleAPISessionStream)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
}

// dashboardWindowMs is what /api/sessions uses to turn start/end into the
// [fromMs, toMs) range handed to the store layer — see its doc comment for
// why this replaced the old rolling-window-plus-Go-side-filter approach.
//
// start/end are parsed in time.Local (not UTC): the dashboard's date picker
// builds these strings from local-midnight Date objects in the browser, and
// the dashboard only ever runs on localhost, so the browser's timezone and
// this server's time.Local are the same machine. These boundaries are
// checked against time.Date(..., time.Local) rather than a hardcoded UTC
// instant so the test asserts the *local* interpretation regardless of the
// machine's offset, and fails under the old time.Parse-in-UTC behaviour on
// any machine where time.Local != UTC. On a CI box where time.Local == UTC,
// this test is a no-op for catching the regression — that's inherent to
// asserting "local" without forcing a non-UTC zone, which would require
// changing dashboardWindowMs's signature just for testability.
func TestDashboardWindowMs_CalendarAnchoredWhenStartEndGiven(t *testing.T) {
	fromMs, toMs := dashboardWindowMs(7, "2026-06-01", "2026-06-03")

	wantFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local).UnixMilli()
	wantTo := time.Date(2026, 6, 4, 0, 0, 0, 0, time.Local).UnixMilli() // exclusive: all of June 3rd included
	if fromMs != wantFrom {
		t.Errorf("fromMs = %d, want %d", fromMs, wantFrom)
	}
	if toMs != wantTo {
		t.Errorf("toMs = %d, want %d", toMs, wantTo)
	}

	// A message that landed at 23:59:59 local time on the end date must
	// fall inside the window — this is exactly the milliseconds-vs-seconds
	// and inclusive-end mistake the store's last_activity comparisons must
	// not make.
	lastMomentOfEndDate := time.Date(2026, 6, 3, 23, 59, 59, 0, time.Local).UnixMilli()
	if lastMomentOfEndDate < fromMs || lastMomentOfEndDate >= toMs {
		t.Errorf("23:59:59 on the end date is not inside [%d, %d)", fromMs, toMs)
	}
}

// Regression test for the UTC/local mismatch bug: the picker builds
// YYYY-MM-DD strings at local midnight, so the server must not silently
// reinterpret them as UTC midnight — doing so shifts the window by the
// machine's UTC offset and misattributes early-morning sessions to the
// wrong day. This assertion is written to actually fail under the old
// time.Parse (UTC) behaviour whenever time.Local != UTC, without hardcoding
// this machine's specific offset: it derives the "wrong" UTC-anchored
// boundary independently and checks the function's result does not match
// it (except in the degenerate case where local IS UTC, which is reported
// rather than silently skipped).
func TestDashboardWindowMs_StartEndAreLocalNotUTC(t *testing.T) {
	fromMs, _ := dashboardWindowMs(7, "2026-06-01", "")

	wantLocalMidnight := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local).UnixMilli()
	if fromMs != wantLocalMidnight {
		t.Errorf("fromMs = %d, want local midnight %d", fromMs, wantLocalMidnight)
	}

	utcMidnight := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if wantLocalMidnight == utcMidnight {
		t.Log("time.Local == UTC on this machine; this test cannot distinguish local-vs-UTC parsing here, but the equality check against wantLocalMidnight above still holds")
	} else if fromMs == utcMidnight {
		t.Errorf("fromMs = %d matches UTC midnight (%d), not local midnight (%d): dashboardWindowMs is parsing start/end as UTC again", fromMs, utcMidnight, wantLocalMidnight)
	}
}

// With no start/end at all, the dashboard's window must be the exact same
// rolling formula the CLI uses for --days N (aggregateSessionsFromStore's
// own cutoff) — that parity is the whole point of Issue 1: "last N days"
// must mean the same session set in both places.
func TestDashboardWindowMs_RollingWhenNoStartEnd(t *testing.T) {
	before := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	fromMs, toMs := dashboardWindowMs(7, "", "")
	after := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()

	if fromMs < before || fromMs > after {
		t.Errorf("fromMs = %d, want within [%d, %d]", fromMs, before, after)
	}
	if toMs != unboundedToMs {
		t.Errorf("toMs = %d, want unbounded (%d)", toMs, unboundedToMs)
	}
}

// Malformed start/end must fall back to the rolling default rather than
// propagating a zero time.Time or an error — time.ParseInLocation's err
// check must gate the assignment exactly like the old time.Parse did.
func TestDashboardWindowMs_MalformedDatesIgnored(t *testing.T) {
	before := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	fromMs, toMs := dashboardWindowMs(7, "not-a-date", "also-not-a-date")
	after := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()

	if fromMs < before || fromMs > after {
		t.Errorf("fromMs = %d, want within [%d, %d] (malformed start should be ignored)", fromMs, before, after)
	}
	if toMs != unboundedToMs {
		t.Errorf("toMs = %d, want unbounded (%d) (malformed end should be ignored)", toMs, unboundedToMs)
	}
}

func TestSessionsStreamBroker_SubscribeBroadcastUnsubscribe(t *testing.T) {
	b := &sessionsStreamBroker{clients: make(map[chan struct{}]struct{})}

	ch1 := b.subscribe()
	ch2 := b.subscribe()

	b.broadcast()

	select {
	case <-ch1:
	default:
		t.Error("ch1 did not receive broadcast")
	}
	select {
	case <-ch2:
	default:
		t.Error("ch2 did not receive broadcast")
	}

	b.unsubscribe(ch1)
	b.broadcast()

	select {
	case <-ch1:
		t.Error("ch1 received broadcast after unsubscribe")
	default:
	}
	select {
	case <-ch2:
	default:
		t.Error("ch2 did not receive broadcast after ch1 unsubscribed")
	}

	b.unsubscribe(ch2)
	if len(b.clients) != 0 {
		t.Errorf("expected 0 clients, got %d", len(b.clients))
	}
}

func TestSessionsStreamBroker_NonBlocking(t *testing.T) {
	b := &sessionsStreamBroker{clients: make(map[chan struct{}]struct{})}
	ch := b.subscribe()
	b.unsubscribe(ch)
	// broadcast should not block even with no clients
	b.broadcast()
}

func TestPISessionDetailMergesToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026-08-03T16-00-00-000Z_test-pi-session.jsonl")
	lines := []string{
		`{"type":"message","timestamp":"2026-08-03T16:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"start prompt"}]}}`,
		`{"type":"message","timestamp":"2026-08-03T16:00:01.000Z","message":{"role":"assistant","model":"test-model","usage":{"input":1,"output":1,"totalTokens":2,"cost":{"total":0.1}},"content":[{"type":"toolCall","id":"call-1","name":"bash","arguments":{"command":"one"}},{"type":"toolCall","id":"call-2","name":"bash","arguments":{"command":"two"}}]}}`,
		`{"type":"message","timestamp":"2026-08-03T16:00:02.000Z","message":{"role":"toolResult","toolCallId":"call-1","toolName":"bash","content":[{"type":"text","text":"result one"}]}}`,
		`{"type":"message","timestamp":"2026-08-03T16:00:03.000Z","message":{"role":"toolResult","toolCallId":"call-2","toolName":"bash","content":[{"type":"text","text":"result two"}]}}`,
		`{"type":"message","timestamp":"2026-08-03T16:00:04.000Z","message":{"role":"assistant","model":"test-model","usage":{"input":1,"output":1,"totalTokens":2,"cost":{"total":0.1}},"content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	detail, err := piSessionDetail(path)
	if err != nil {
		t.Fatalf("piSessionDetail() = %v", err)
	}
	if len(detail.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(detail.Steps))
	}
	if detail.Steps[0].UserPrompt != "start prompt" {
		t.Fatalf("UserPrompt = %q, want %q", detail.Steps[0].UserPrompt, "start prompt")
	}
	if len(detail.Steps[0].ToolCalls) != 2 {
		t.Fatalf("first-step tool calls = %d, want 2", len(detail.Steps[0].ToolCalls))
	}
	if len(detail.Steps[1].ToolCalls) != 0 {
		t.Fatalf("second-step tool calls = %d, want 0", len(detail.Steps[1].ToolCalls))
	}
	if detail.Steps[0].ToolCalls[0].ID != "call-1" {
		t.Fatalf("first tool ID = %q, want call-1", detail.Steps[0].ToolCalls[0].ID)
	}
	if string(detail.Steps[0].ToolCalls[0].Output) != `"result one"` {
		t.Fatalf("first tool output = %s, want %q", detail.Steps[0].ToolCalls[0].Output, `"result one"`)
	}
	if string(detail.Steps[0].ToolCalls[1].Output) != `"result two"` {
		t.Fatalf("second tool output = %s, want %q", detail.Steps[0].ToolCalls[1].Output, `"result two"`)
	}
}

func TestGetSessionDetailFromStoreKeepsLeadingPrompt(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tokeneks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	setTokeneksStore(st)
	defer setTokeneksStore(nil)

	if err := st.IngestSession(context.Background(), store.ParsedSession{
		Session: store.Session{Agent: "claude", SessionID: "prompt-test", CreatedAt: 1, LastActivity: 2},
		Messages: []store.ParsedMessage{
			{Message: store.Message{Agent: "claude", SessionID: "prompt-test", MsgIndex: 0, Role: store.RoleUser, Content: "starting prompt", CreatedAt: 1}},
			{Message: store.Message{Agent: "claude", SessionID: "prompt-test", MsgIndex: 1, Role: store.RoleAssistant, Response: "answer", CreatedAt: 2}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	detail, err := getSessionDetailFromStore(context.Background(), "Claude", "prompt-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].UserPrompt != "starting prompt" {
		t.Fatalf("steps = %+v, want leading prompt attached to first step", detail.Steps)
	}
}

func TestMarkdownExportFormatting(t *testing.T) {
	detail := &SessionDetail{
		Agent:   "OpenCode",
		ID:      "ses_test",
		Project: "tokeneks",
		Steps: []StepInfo{{
			Step:       1,
			UserPrompt: "Implement the export button",
			Response:   "Done",
			Cost:       0.25,
			ToolCalls: []ToolCallInfo{{
				Name:   "bash",
				Input:  json.RawMessage(`{"cmd":"pwd"}`),
				Output: json.RawMessage(`"/tmp"`),
			}},
		}},
	}

	var markdown strings.Builder
	appendMarkdownSession(&markdown, nil, detail, 1, false, make(map[sessKey]bool))
	got := markdown.String()
	for _, want := range []string{
		"# OpenCode session ses\\_test",
		"### User prompt\n\nImplement the export button",
		"### Response\n\nDone",
		"### Tool: bash",
		"#### Input\n\n```\n{\n  \"cmd\": \"pwd\"\n}",
		"#### Output\n\n```\n/tmp",
		"- Total cost: $0.2500",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown export is missing %q:\n%s", want, got)
		}
	}
}

func TestMarkdownDownloadFilename_SanitizesPath(t *testing.T) {
	got := markdownDownloadFilename("OpenCode", "ses/ABC")
	if got != "session-opencode-ses-abc.md" {
		t.Errorf("markdownDownloadFilename() = %q, want %q", got, "session-opencode-ses-abc.md")
	}
}

// The web dashboard and `total` must never disagree about a session's
// Paid/Ideal — that agreement is the whole point of sharing
// computeSessionPricing between them (see totalsByAgent's doc comment in
// main.go). This ingests the same fixture totalsByAgent's own tests use
// (a Claude session that switches models mid-run, so the ideal-cache
// carry-forward actually matters) and checks the two surfaces agree to
// within floating-point noise from summation order.
func TestGatherWebSessionsFromStore_AgreesWithTotalsByAgent(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "claude", "s1", []testStep{
		{Model: "claude-sonnet-5", Step: compute.StepData{Input: 20000, Output: 1000}},
		{Model: "claude-opus-5", Step: compute.StepData{Input: 20000, Output: 1000}},
	})
	ingestTotalTestSession(t, st, "opencode", "s2", []testStep{
		loggedStep("Kimi K2.6", 5000, 1000, 500, 1.23),
	})
	ingestTotalTestSession(t, st, "pi", "s3", []testStep{
		loggedStep("Kimi K2.6", 3000, 500, 200, 0.42),
	})

	ctx := context.Background()
	fromMs, toMs := time.Now().Add(-30*24*time.Hour).UnixMilli(), unboundedToMs
	webSessions, err := gatherWebSessionsFromStore(ctx, fromMs, toMs)
	if err != nil {
		t.Fatalf("gatherWebSessionsFromStore: %v", err)
	}
	rows, _, err := totalsByAgent(ctx, 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}

	webAgentLabel := map[string]string{"Claude": "CLAUDE", "OpenCode": "OC", "PI": "PI"}
	type totals struct{ paid, ideal float64 }
	gotByAgent := map[string]totals{}
	for _, ws := range webSessions {
		label := webAgentLabel[ws.Agent]
		e := gotByAgent[label]
		e.paid += ws.TotalCost
		e.ideal += ws.Ideal
		gotByAgent[label] = e
	}

	const tol = 1e-9
	for _, row := range rows {
		got := gotByAgent[row.Label]
		if !approxEqual(got.paid, row.Actual, tol) {
			t.Errorf("%s: web Paid = %v, totalsByAgent Actual = %v", row.Label, got.paid, row.Actual)
		}
		if !approxEqual(got.ideal, row.Ideal, tol) {
			t.Errorf("%s: web Ideal = %v, totalsByAgent Ideal = %v", row.Label, got.ideal, row.Ideal)
		}
	}
}

// Same agreement check as above, but for the session detail view (the other
// caller of computeSessionPricing) instead of the session list.
func TestGetSessionDetailFromStore_AgreesWithTotalsByAgent(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "claude", "s1", []testStep{
		{Model: "claude-sonnet-5", Step: compute.StepData{Input: 20000, Output: 1000}},
		{Model: "claude-opus-5", Step: compute.StepData{Input: 20000, Output: 1000}},
	})

	ctx := context.Background()
	detail, err := getSessionDetailFromStore(ctx, "claude", "s1")
	if err != nil {
		t.Fatalf("getSessionDetailFromStore: %v", err)
	}
	rows, _, err := totalsByAgent(ctx, 30)
	if err != nil {
		t.Fatalf("totalsByAgent: %v", err)
	}
	claude := agentTotalRow(t, rows, "CLAUDE")

	const tol = 1e-9
	if !approxEqual(detail.TotalCost, claude.Actual, tol) {
		t.Errorf("detail.TotalCost = %v, totalsByAgent Actual = %v", detail.TotalCost, claude.Actual)
	}
	if !approxEqual(detail.Ideal, claude.Ideal, tol) {
		t.Errorf("detail.Ideal = %v, totalsByAgent Ideal = %v", detail.Ideal, claude.Ideal)
	}
	wantOverpay := claude.Actual - claude.Ideal
	if wantOverpay < 0 {
		wantOverpay = 0
	}
	if !approxEqual(detail.Overpay, wantOverpay, tol) {
		t.Errorf("detail.Overpay = %v, want %v", detail.Overpay, wantOverpay)
	}
}

// A session with a step whose model has no resolvable rate and no logged
// cost must not silently read as "0% overpay" — its overpay is partially
// unknown, not zero, and the web layer must say so rather than pricing the
// unpriced tokens at some invented rate (or worse, dropping them without a
// trace).
func TestGatherWebSessionsFromStore_FlagsPartiallyUnpriced(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "opencode", "s1", []testStep{
		loggedStep("Kimi K2.6", 5000, 1000, 500, 1.0),
		step("fake-model-no-price-xyz", 2000, 0, 100), // no rate, no logged cost
	})

	sessions, err := gatherWebSessionsFromStore(context.Background(), time.Now().Add(-30*24*time.Hour).UnixMilli(), unboundedToMs)
	if err != nil {
		t.Fatalf("gatherWebSessionsFromStore: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	ws := sessions[0]
	if !ws.PartiallyUnpriced {
		t.Error("expected PartiallyUnpriced = true")
	}
	if ws.UnpricedTokens == 0 {
		t.Error("expected UnpricedTokens > 0")
	}
	// Only the priced, logged step contributes — the unpriced step is
	// excluded outright since it has no logged cost to fall back on (see
	// unpricedModel in main.go).
	if ws.TotalCost != 1.0 {
		t.Errorf("TotalCost = %v, want 1.0 (only the priced/logged step)", ws.TotalCost)
	}
}

func TestOCSessions_UsesStoredStepFinishCost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dbPath := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() = %v", err)
	}
	resetOCDBForTest(t)

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() = %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT, model TEXT, time_created INTEGER, tokens_input INTEGER, tokens_output INTEGER, tokens_cache_read INTEGER, tokens_cache_write INTEGER, parent_id TEXT, cost REAL, project_id TEXT);`,
		`CREATE TABLE project (id TEXT PRIMARY KEY, name TEXT, worktree TEXT);`,
		`CREATE TABLE part (session_id TEXT, time_created INTEGER, data TEXT);`,
		`INSERT INTO session (id, title, model, time_created, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, parent_id, cost, project_id) VALUES ('sess-1', 'Stored cost wins', '{"id":"Claude Sonnet 4.6","providerID":"nexos-ai"}', 1710000000000, 10, 20, 30, 40, '', 9.99, '');`,
		`INSERT INTO part (session_id, time_created, data) VALUES ('sess-1', 1710000001000, '{"type":"step-finish","cost":4.22,"tokens":{"input":10,"output":20,"cache":{"read":30,"write":40}}}');`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("db.Exec(%q) = %v", stmt, err)
		}
	}

	sessions, err := ocSessions(3650, "")
	if err != nil {
		t.Fatalf("ocSessions() = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ocSessions() len=%d, want 1", len(sessions))
	}
	if sessions[0].Cost != 4.22 {
		t.Fatalf("ocSessions()[0].Cost = %v, want 4.22", sessions[0].Cost)
	}
}
