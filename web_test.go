package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"tokeneks/compute"

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
		`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT, model TEXT, time_created INTEGER, tokens_input INTEGER, tokens_output INTEGER, tokens_cache_read INTEGER, tokens_cache_write INTEGER, parent_id TEXT, cost REAL);`,
		`CREATE TABLE part (session_id TEXT, time_created INTEGER, data TEXT);`,
		`INSERT INTO session (id, title, model, time_created, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, parent_id, cost) VALUES ('sess-1', 'Stored cost wins', '{"id":"Claude Sonnet 4.6","providerID":"nexos-ai"}', 1710000000000, 10, 20, 30, 40, '', 9.99);`,
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
