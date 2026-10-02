package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpen_CreatesSchema(t *testing.T) {
	st := openTestStore(t)
	rows, err := st.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got[name] = true
	}
	for _, want := range []string{"session", "message", "tool_call"} {
		if !got[want] {
			t.Errorf("missing table %q", want)
		}
	}
}

// TestMigrate_AddsCacheWrite1hColumn simulates a database created before the
// cache-write TTL split shipped: the message table exists but has no
// cache_write_1h column. Open() must add it (defaulting existing rows to 0,
// which is the harmless pre-fix behavior) rather than failing.
func TestMigrate_AddsCacheWrite1hColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE session (agent TEXT, session_id TEXT, project TEXT, parent_id TEXT,
		  created_at INTEGER, last_activity INTEGER, PRIMARY KEY (agent, session_id));
		CREATE TABLE message (
		  id INTEGER PRIMARY KEY AUTOINCREMENT, agent TEXT, session_id TEXT, msg_index INTEGER,
		  role TEXT, content TEXT, model TEXT, provider TEXT,
		  input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		  cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0,
		  cost REAL NOT NULL DEFAULT 0, stop_reason TEXT, thinking TEXT, response TEXT,
		  tool_call_id TEXT, created_at INTEGER NOT NULL);
		INSERT INTO message
		  (agent, session_id, msg_index, role, content, model, provider,
		   stop_reason, thinking, response, tool_call_id, cache_write, created_at)
		  VALUES ('claude', 's1', 0, 'assistant', '', '', '', '', '', '', '', 42, 1000);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-migration db: %v", err)
	}
	defer func() { _ = st.Close() }()

	msgs, err := st.GetMessages(context.Background(), "claude", "s1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs)=%d, want 1", len(msgs))
	}
	if msgs[0].CacheWrite != 42 {
		t.Errorf("CacheWrite = %d, want 42 (untouched by migration)", msgs[0].CacheWrite)
	}
	if msgs[0].CacheWrite1h != 0 {
		t.Errorf("CacheWrite1h = %d, want 0 for a pre-migration row", msgs[0].CacheWrite1h)
	}
}

// TestMigrate_AddsModelPriceEffectiveWindow simulates a database created
// before model_price grew a validity window: the table exists under the
// old (provider, model) primary key, with no effective_from/effective_to
// columns. Open() must rebuild it (see migrateModelPriceEffectiveWindow)
// without losing any row, and every pre-existing row must come back fully
// open (effective_from=0, effective_to=0) — the same rate it already
// applied at every timestamp, so the migration itself changes no report's
// numbers.
func TestMigrate_AddsModelPriceEffectiveWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE model_price (
		  provider       TEXT    NOT NULL,
		  model          TEXT    NOT NULL,
		  name           TEXT,
		  input          REAL    NOT NULL DEFAULT 0,
		  output         REAL    NOT NULL DEFAULT 0,
		  cache_read     REAL    NOT NULL DEFAULT 0,
		  cache_write    REAL    NOT NULL DEFAULT 0,
		  cache_write_1h REAL    NOT NULL DEFAULT 0,
		  source         TEXT    NOT NULL,
		  updated_at     INTEGER NOT NULL,
		  PRIMARY KEY (provider, model)
		);
		INSERT INTO model_price (provider, model, name, input, output, cache_read, cache_write, cache_write_1h, source, updated_at)
		  VALUES ('anthropic', 'claude-opus-5', 'Opus 5', 5.0, 25.0, 0.5, 6.25, 10.0, 'models.dev', 1000);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-migration db: %v", err)
	}
	defer func() { _ = st.Close() }()

	prices, err := st.GetModelPrices(context.Background(), "anthropic")
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("len(prices)=%d, want 1: %+v", len(prices), prices)
	}
	p := prices[0]
	if p.Model != "claude-opus-5" || p.Input != 5.0 || p.Output != 25.0 || p.CacheWrite1h != 10.0 {
		t.Errorf("row corrupted by migration: %+v", p)
	}
	if p.EffectiveFrom != 0 || p.EffectiveTo != 0 {
		t.Errorf("EffectiveFrom/To = %d/%d, want 0/0 (fully open) for a pre-migration row", p.EffectiveFrom, p.EffectiveTo)
	}

	// Re-opening an already-migrated database must be a no-op, not a
	// second attempt to rename a table that no longer exists.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotency): %v", err)
	}
	defer func() { _ = st2.Close() }()
	prices2, err := st2.GetModelPrices(context.Background(), "anthropic")
	if err != nil {
		t.Fatalf("GetModelPrices after second Open: %v", err)
	}
	if len(prices2) != 1 || prices2[0].Model != "claude-opus-5" {
		t.Errorf("row lost or duplicated across idempotent migration: %+v", prices2)
	}
}

// A (provider, model) pair with more than one effective window (a derived
// rate that changed mid-window, see prices_derive.go) must store and
// retrieve every window, not just one — the primary key extension to
// effective_from is what makes that possible.
func TestUpsertModelPrices_MultipleWindowsPerModel(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	err := st.UpsertModelPrices(ctx, []ModelPrice{
		{Provider: "derived:opencode", Model: "GPT 5.6 Luna", Input: 5.0, Output: 25.0, Source: "derived", UpdatedAt: 1, EffectiveFrom: 0, EffectiveTo: 1000},
		{Provider: "derived:opencode", Model: "GPT 5.6 Luna", Input: 1.1, Output: 6.6, Source: "derived", UpdatedAt: 1, EffectiveFrom: 1000, EffectiveTo: 0},
	})
	if err != nil {
		t.Fatalf("UpsertModelPrices: %v", err)
	}

	prices, err := st.GetModelPrices(ctx, "derived:opencode")
	if err != nil {
		t.Fatalf("GetModelPrices: %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("len(prices)=%d, want 2 windows: %+v", len(prices), prices)
	}
	if prices[0].EffectiveFrom != 0 || prices[0].EffectiveTo != 1000 || prices[0].Input != 5.0 {
		t.Errorf("window 0 = %+v, want the pre-shift window", prices[0])
	}
	if prices[1].EffectiveFrom != 1000 || prices[1].EffectiveTo != 0 || prices[1].Input != 1.1 {
		t.Errorf("window 1 = %+v, want the post-shift window", prices[1])
	}
}

func TestUpsertSession_InsertAndUpdate(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	s1 := Session{Agent: "claude", SessionID: "s1", Project: "p1", CreatedAt: 1000, LastActivity: 2000}
	if err := st.UpsertSession(ctx, s1); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	got, err := st.GetSession(ctx, "claude", "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Project != "p1" || got.LastActivity != 2000 {
		t.Errorf("GetSession = %+v", got)
	}

	// update
	s1.Project = "p2"
	s1.LastActivity = 3000
	if err := st.UpsertSession(ctx, s1); err != nil {
		t.Fatalf("UpsertSession update: %v", err)
	}
	got, _ = st.GetSession(ctx, "claude", "s1")
	if got.Project != "p2" || got.LastActivity != 3000 {
		t.Errorf("after update = %+v", got)
	}
}

func TestIngestSession_InsertsMessagesAndToolCalls(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	ps := ParsedSession{
		Session: Session{Agent: "claude", SessionID: "s1", Project: "p", CreatedAt: 1000, LastActivity: 5000},
		Messages: []ParsedMessage{
			{Message: Message{Agent: "claude", SessionID: "s1", MsgIndex: 0, Role: RoleUser, Content: "fix bug", CreatedAt: 1000}},
			{Message: Message{Agent: "claude", SessionID: "s1", MsgIndex: 1, Role: RoleAssistant, Model: "claude-sonnet-4-6", Content: "ok", InputTokens: 100, OutputTokens: 50, Cost: 0.01, CreatedAt: 2000},
				ToolCalls: []ToolCall{
					{CallID: "c1", Name: "read", Input: `{"path":"/x"}`},
				},
			},
			{Message: Message{Agent: "claude", SessionID: "s1", MsgIndex: 2, Role: RoleTool, Content: "file contents", ToolCallID: "c1", CreatedAt: 2500}},
		},
	}
	if err := st.IngestSession(ctx, ps); err != nil {
		t.Fatalf("IngestSession: %v", err)
	}

	msgs, err := st.GetMessages(ctx, "claude", "s1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len(msgs)=%d, want 3", len(msgs))
	}
	if msgs[0].Role != RoleUser || msgs[1].Role != RoleAssistant || msgs[2].Role != RoleTool {
		t.Errorf("roles = %s,%s,%s", msgs[0].Role, msgs[1].Role, msgs[2].Role)
	}
	if msgs[2].ToolCallID != "c1" {
		t.Errorf("tool_call_id = %q, want c1", msgs[2].ToolCallID)
	}

	tcs, err := st.GetToolCalls(ctx, msgs[1].ID)
	if err != nil {
		t.Fatalf("GetToolCalls: %v", err)
	}
	if len(tcs) != 1 || tcs[0].CallID != "c1" || tcs[0].Name != "read" {
		t.Errorf("tool_calls = %+v", tcs)
	}

	st2, err := st.SessionStats(ctx, "claude", "s1")
	if err != nil {
		t.Fatalf("SessionStats: %v", err)
	}
	if st2.MessageCount != 3 || st2.ToolCallCount != 1 || st2.InputTokens != 100 || st2.OutputTokens != 50 {
		t.Errorf("SessionStats = %+v", st2)
	}
}

func TestIngestSession_ReplacesExisting(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	mk := func(n int) ParsedSession {
		msgs := make([]ParsedMessage, n)
		for i := 0; i < n; i++ {
			msgs[i] = ParsedMessage{Message: Message{Agent: "claude", SessionID: "s1", MsgIndex: i, Role: RoleUser, Content: "x", CreatedAt: int64(1000 + i)}}
		}
		return ParsedSession{Session: Session{Agent: "claude", SessionID: "s1", CreatedAt: 1000, LastActivity: 9999}, Messages: msgs}
	}
	if err := st.IngestSession(ctx, mk(5)); err != nil {
		t.Fatalf("IngestSession 5: %v", err)
	}
	if err := st.IngestSession(ctx, mk(2)); err != nil {
		t.Fatalf("IngestSession 2: %v", err)
	}
	msgs, _ := st.GetMessages(ctx, "claude", "s1")
	if len(msgs) != 2 {
		t.Errorf("after replace len=%d, want 2", len(msgs))
	}
}

func TestGetSessionMTimes(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	ingest := func(agent, id string, mtime int64) {
		ps := ParsedSession{
			Session:  Session{Agent: agent, SessionID: id, CreatedAt: mtime, LastActivity: mtime, SourceMTime: mtime},
			Messages: []ParsedMessage{{Message: Message{Agent: agent, SessionID: id, MsgIndex: 0, Role: RoleUser, Content: "x", CreatedAt: mtime}}},
		}
		if err := st.IngestSession(ctx, ps); err != nil {
			t.Fatalf("IngestSession(%s/%s): %v", agent, id, err)
		}
	}
	ingest("claude", "a", 100)
	ingest("claude", "b", 200)
	ingest("opencode", "x", 300)

	got, err := st.GetSessionMTimes(ctx, "claude")
	if err != nil {
		t.Fatalf("GetSessionMTimes: %v", err)
	}
	if len(got) != 2 || got["a"] != 100 || got["b"] != 200 {
		t.Errorf("claude mtimes = %+v, want {a:100, b:200}", got)
	}
	got, err = st.GetSessionMTimes(ctx, "opencode")
	if err != nil {
		t.Fatalf("GetSessionMTimes: %v", err)
	}
	if len(got) != 1 || got["x"] != 300 {
		t.Errorf("opencode mtimes = %+v, want {x:300}", got)
	}
	got, err = st.GetSessionMTimes(ctx, "pi")
	if err != nil {
		t.Fatalf("GetSessionMTimes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("pi mtimes = %+v, want empty", got)
	}
}

func TestGetSessions_FilterByAgentAndTime(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	sessions := []Session{
		{Agent: "claude", SessionID: "a", CreatedAt: 1000, LastActivity: 1500},
		{Agent: "claude", SessionID: "b", CreatedAt: 2000, LastActivity: 2500},
		{Agent: "pi", SessionID: "c", CreatedAt: 3000, LastActivity: 3500},
	}
	for _, s := range sessions {
		if err := st.UpsertSession(ctx, s); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}

	got, err := st.GetSessions(ctx, SessionFilter{Agent: "claude"})
	if err != nil {
		t.Fatalf("GetSessions: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("claude filter len=%d, want 2", len(got))
	}

	got, err = st.GetSessions(ctx, SessionFilter{MinCreatedAt: 2000})
	if err != nil {
		t.Fatalf("GetSessions min: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("min=2000 filter len=%d, want 2", len(got))
	}
}
