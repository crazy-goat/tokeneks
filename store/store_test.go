package store

import (
	"context"
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

func TestDeleteSession_Cascades(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	ps := ParsedSession{
		Session: Session{Agent: "claude", SessionID: "s1", CreatedAt: 1, LastActivity: 2},
		Messages: []ParsedMessage{
			{Message: Message{Agent: "claude", SessionID: "s1", MsgIndex: 0, Role: RoleAssistant, CreatedAt: 1},
				ToolCalls: []ToolCall{{CallID: "c1", Name: "bash"}}},
		},
	}
	if err := st.IngestSession(ctx, ps); err != nil {
		t.Fatalf("IngestSession: %v", err)
	}
	if err := st.DeleteSession(ctx, "claude", "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	msgs, _ := st.GetMessages(ctx, "claude", "s1")
	if len(msgs) != 0 {
		t.Errorf("after delete len(msgs)=%d, want 0", len(msgs))
	}
	var orphanCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM tool_call`).Scan(&orphanCount); err != nil {
		t.Fatalf("query: %v", err)
	}
	if orphanCount != 0 {
		t.Errorf("orphan tool_calls = %d, want 0 (CASCADE failed)", orphanCount)
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
