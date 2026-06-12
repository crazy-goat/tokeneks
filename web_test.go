package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestAppendPIChildSessions_NestedChildrenKeepParentID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sessionsDir := filepath.Join(home, ".pi", "agent", "sessions", "--Users-piotr-halas-work-tokeneks--")
	projectDir := filepath.Join(sessionsDir, "proj")
	rootFile := filepath.Join(projectDir, "2026-06-11_root.jsonl")
	rootSessionDir := strings.TrimSuffix(rootFile, ".jsonl")

	if err := os.MkdirAll(filepath.Join(rootSessionDir, "child1", "run-0", "session", "child2", "run-0"), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() = %v", err)
	}

	child1File := filepath.Join(rootSessionDir, "child1", "run-0", "session.jsonl")
	child2File := filepath.Join(rootSessionDir, "child1", "run-0", "session", "child2", "run-0", "session.jsonl")

	writePIJSONL := func(path, model string) {
		t.Helper()
		content := []byte(`{"type":"message","timestamp":"2026-06-11T00:00:00Z","message":{"role":"assistant","provider":"nexos-ai","model":"` + model + `","usage":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":3,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"content":[{"type":"text","text":"hi"}]}}
`)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatalf("os.WriteFile(%s) = %v", path, err)
		}
	}
	writePIJSONL(child1File, "child1-model")
	writePIJSONL(child2File, "child2-model")

	sessions := []WebSession{{Agent: "PI", ID: "root", Project: "root"}}
	appendPIChildSessions(&sessions, "root", "root", rootFile)

	var child1, child2 *WebSession
	for i := range sessions {
		s := &sessions[i]
		switch s.ID {
		case "child1":
			child1 = s
		case "child2":
			child2 = s
		}
	}

	if child1 == nil {
		t.Fatalf("first-level child PI session not found in web sessions: %+v", sessions)
	}
	if child2 == nil {
		t.Fatalf("nested child PI session not found in web sessions: %+v", sessions)
	}
	if child1.ParentID != "root" {
		t.Fatalf("child1.ParentID = %q, want %q", child1.ParentID, "root")
	}
	if child2.ParentID != "child1" {
		t.Fatalf("child2.ParentID = %q, want %q", child2.ParentID, "child1")
	}
	if !child1.IsSubsession || !child2.IsSubsession {
		t.Fatalf("expected nested children to be marked as subsessions: child1=%v child2=%v", child1.IsSubsession, child2.IsSubsession)
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
