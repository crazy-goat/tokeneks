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
	"tokeneks/compute"
	"tokeneks/store"

	_ "github.com/mattn/go-sqlite3"
)

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

func TestFilterWebSessionsByDateRange_UsesDateOrLastMessage(t *testing.T) {
	sessions := []WebSession{
		{ID: "date-only", Date: "2026-06-11 10:00", LastMessage: "2026-06-10 09:00"},
		{ID: "last-only", Date: "2026-06-09 10:00", LastMessage: "2026-06-11 18:30:00"},
		{ID: "outside", Date: "2026-06-01 10:00", LastMessage: "2026-06-01 11:00:00"},
	}

	filtered := filterWebSessionsByDateRange(sessions, "2026-06-11", "2026-06-11")
	if len(filtered) != 2 {
		t.Fatalf("filterWebSessionsByDateRange() len=%d, want 2", len(filtered))
	}
	seen := map[string]bool{}
	for _, s := range filtered {
		seen[s.ID] = true
	}
	if !seen["date-only"] || !seen["last-only"] {
		t.Fatalf("filterWebSessionsByDateRange() missing expected sessions: %+v", seen)
	}
	if seen["outside"] {
		t.Fatalf("filterWebSessionsByDateRange() included out-of-range session")
	}
}

func TestPIStepWebCost_UsesStoredUsageCost(t *testing.T) {
	step := piSessionStep{
		Model: "kimi-k2.6",
		Step: compute.StepData{
			Input:     399855,
			CacheRead: 71936711,
			Output:    150179,
		},
		Cost: 12.49045201,
	}

	got := piStepWebCost(step)
	if got != step.Cost {
		t.Fatalf("piStepWebCost() = %v, want stored cost %v", got, step.Cost)
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
	defer st.Close()
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
