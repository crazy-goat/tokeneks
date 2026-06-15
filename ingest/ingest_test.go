package ingest

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"tokeneks/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestClaudeSource_DiscoversSessions(t *testing.T) {
	dir := t.TempDir()
	// fake structure: <root>/<project>/<sessionID>.jsonl
	proj := filepath.Join(dir, "proj1")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"sess-a.jsonl": "x",
		"sess-b.jsonl": "y",
		"subagents/foo.jsonl": "z", // also picked up — separate session
	}
	for rel, content := range files {
		fp := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := NewClaudeSource(dir)
	refs, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(refs) != 3 {
		t.Errorf("len(refs)=%d, want 3", len(refs))
	}
	ids := map[string]bool{}
	for _, r := range refs {
		ids[r.SessionID] = true
	}
	for _, want := range []string{"sess-a", "sess-b", "foo"} {
		if !ids[want] {
			t.Errorf("missing session %q", want)
		}
	}
}

func TestPiSource_DiscoversSessions(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "--Users-piotr--")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{
		"2026-06-15_aaa.jsonl",
		"2026-06-15_bbb.jsonl",
		"sub/session.jsonl", // nested → id = "sub"
	}
	for _, rel := range files {
		fp := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := NewPiSource(dir)
	refs, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range refs {
		ids[r.SessionID] = true
	}
	for _, want := range []string{"aaa", "bbb", "sub"} {
		if !ids[want] {
			t.Errorf("missing pi session %q (got %v)", want, ids)
		}
	}
}

func TestSources_MissingRoot_NoError(t *testing.T) {
	for name, src := range map[string]Source{
		"claude":   NewClaudeSource("/nonexistent/path"),
		"pi":       NewPiSource("/nonexistent/path"),
		"opencode": NewOpenCodeSource("/nonexistent/file.db"),
	} {
		t.Run(name, func(t *testing.T) {
			refs, err := src.Discover(context.Background())
			if err != nil {
				t.Errorf("Discover: %v", err)
			}
			if len(refs) != 0 {
				t.Errorf("len(refs)=%d, want 0", len(refs))
			}
		})
	}
}

func TestIngestor_Sync_CallsParserForEachRef(t *testing.T) {
	st := openStore(t)
	parseCalls := 0
	storedCount := 0

	src := &mockSource{
		agent: "test",
		refs: []SessionRef{
			{Agent: "test", SessionID: "s1", Source: "/p1"},
			{Agent: "test", SessionID: "s2", Source: "/p2"},
		},
	}
	parser := func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
		parseCalls++
		return store.ParsedSession{
			Session: store.Session{Agent: "test", SessionID: ref.SessionID, CreatedAt: 1, LastActivity: 2},
			Messages: []store.ParsedMessage{
				{Message: store.Message{Agent: "test", SessionID: ref.SessionID, MsgIndex: 0, Role: store.RoleUser, Content: "hi", CreatedAt: 1}},
			},
		}, nil
	}

	ing := &Ingestor{
		Store:  st,
		Agents: []string{"test"},
		SourceFor: map[string]Source{"test": src},
		ParserFor: map[string]Parser{"test": parser},
	}
	res, err := ing.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if parseCalls != 2 {
		t.Errorf("parseCalls=%d, want 2", parseCalls)
	}
	if res.Discovered != 2 || res.Ingested != 2 || res.Errors != 0 {
		t.Errorf("res=%+v", res)
	}
	storedCount, _ = st.CountSessions(context.Background(), "test")
	if storedCount != 2 {
		t.Errorf("storedCount=%d, want 2", storedCount)
	}

	// Run again — should upsert, not duplicate.
	res, _ = ing.Sync(context.Background())
	if res.Ingested != 2 {
		t.Errorf("re-sync Ingested=%d, want 2 (should be idempotent)", res.Ingested)
	}
	storedCount, _ = st.CountSessions(context.Background(), "test")
	if storedCount != 2 {
		t.Errorf("after re-sync storedCount=%d, want 2", storedCount)
	}
}

func TestIngestor_Sync_ReportsErrors(t *testing.T) {
	st := openStore(t)
	src := &mockSource{
		agent: "test",
		refs:  []SessionRef{{Agent: "test", SessionID: "ok"}},
	}
	parser := func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
		return store.ParsedSession{}, errIngest
	}
	ing := &Ingestor{
		Store:     st,
		Agents:    []string{"test"},
		SourceFor: map[string]Source{"test": src},
		ParserFor: map[string]Parser{"test": parser},
	}
	res, err := ing.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Errors != 1 {
		t.Errorf("Errors=%d, want 1", res.Errors)
	}
}

func TestIngestor_Sync_CleansUpStaleEmptyRows(t *testing.T) {
	st := openStore(t)

	// Pre-insert an empty session (stale row).
	if err := st.UpsertSession(context.Background(), store.Session{
		Agent: "test", SessionID: "stale", CreatedAt: 1, LastActivity: 1, SourceMTime: 100,
	}); err != nil {
		t.Fatal(err)
	}

	src := &mockSource{
		agent: "test",
		refs: []SessionRef{
			{Agent: "test", SessionID: "stale", MTime: 100}, // same mtime
			{Agent: "test", SessionID: "fresh", MTime: 100},
		},
	}
	parser := func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
		if ref.SessionID == "stale" {
			// Source now has no data.
			return store.ParsedSession{}, errIngest
		}
		return store.ParsedSession{
			Session: store.Session{Agent: ref.Agent, SessionID: ref.SessionID, CreatedAt: 1, LastActivity: 2, SourceMTime: ref.MTime},
			Messages: []store.ParsedMessage{
				{Message: store.Message{Agent: ref.Agent, SessionID: ref.SessionID, MsgIndex: 0, Role: store.RoleUser, Content: "x", CreatedAt: 1}},
			},
		}, nil
	}
	ing := &Ingestor{
		Store:     st,
		Agents:    []string{"test"},
		SourceFor: map[string]Source{"test": src},
		ParserFor: map[string]Parser{"test": parser},
	}
	res, _ := ing.Sync(context.Background())

	// "stale" should be re-parsed (because it's empty), parser fails, row is deleted.
	// "fresh" should be ingested normally.
	if _, err := st.GetSession(context.Background(), "test", "stale"); err == nil {
		t.Errorf("stale session should have been deleted")
	}
	if _, err := st.GetSession(context.Background(), "test", "fresh"); err != nil {
		t.Errorf("fresh session should exist: %v", err)
	}
	if res.Ingested != 1 {
		t.Errorf("Ingested=%d, want 1", res.Ingested)
	}
	if res.Errors != 1 {
		t.Errorf("Errors=%d, want 1", res.Errors)
	}
}

func TestIngestor_Sync_SkipsUnchangedByMTime(t *testing.T) {
	st := openStore(t)

	// First discovery: mtime=100
	src := &mockSource{
		agent: "test",
		refs:  []SessionRef{{Agent: "test", SessionID: "s1", MTime: 100}},
	}
	parser := func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
		return store.ParsedSession{
			Session: store.Session{Agent: ref.Agent, SessionID: ref.SessionID, CreatedAt: 1, LastActivity: 2, SourceMTime: ref.MTime},
			Messages: []store.ParsedMessage{
				{Message: store.Message{Agent: ref.Agent, SessionID: ref.SessionID, MsgIndex: 0, Role: store.RoleUser, Content: "hi", CreatedAt: 1}},
			},
		}, nil
	}
	ing := &Ingestor{
		Store:     st,
		Agents:    []string{"test"},
		SourceFor: map[string]Source{"test": src},
		ParserFor: map[string]Parser{"test": parser},
	}

	// First sync: ingests.
	res, err := ing.Sync(context.Background())
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if res.Ingested != 1 || res.Skipped != 0 {
		t.Errorf("first sync: %+v, want Ingested=1 Skipped=0", res)
	}

	// Second sync with same mtime: should skip.
	res, err = ing.Sync(context.Background())
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if res.Ingested != 0 || res.Skipped != 1 {
		t.Errorf("second sync: %+v, want Ingested=0 Skipped=1", res)
	}

	// Third sync with newer mtime: should re-ingest.
	src.refs = []SessionRef{{Agent: "test", SessionID: "s1", MTime: 200}}
	res, err = ing.Sync(context.Background())
	if err != nil {
		t.Fatalf("third Sync: %v", err)
	}
	if res.Ingested != 1 || res.Skipped != 0 {
		t.Errorf("third sync: %+v, want Ingested=1 Skipped=0", res)
	}

	// Force re-ingest: should re-ingest even with old mtime.
	src.refs = []SessionRef{{Agent: "test", SessionID: "s1", MTime: 50}}
	ing.Force = true
	res, err = ing.Sync(context.Background())
	if err != nil {
		t.Fatalf("force Sync: %v", err)
	}
	if res.Ingested != 1 {
		t.Errorf("force sync: %+v, want Ingested=1", res)
	}
}

type mockSource struct {
	agent string
	refs  []SessionRef
	root  string
}

func (m *mockSource) Agent() string { return m.agent }
func (m *mockSource) Root() string  { return m.root }
func (m *mockSource) Discover(ctx context.Context) ([]SessionRef, error) {
	out := make([]SessionRef, len(m.refs))
	copy(out, m.refs)
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

var errIngest = &ingestErr{"boom"}

type ingestErr struct{ s string }

func (e *ingestErr) Error() string { return e.s }
