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
		"sess-a.jsonl":        "x",
		"sess-b.jsonl":        "y",
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

func TestJSONLMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	// A line with no "timestamp" field: the old marker returned 0 for
	// these, which made unrelated sessions compare equal.
	if err := os.WriteFile(path, []byte("{\"type\":\"summary\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := jsonlMarker(path, nil)
	if first == 0 {
		t.Fatal("jsonlMarker() = 0 for an existing file")
	}
	if again := jsonlMarker(path, nil); again != first {
		t.Fatalf("jsonlMarker() unstable for an unchanged file: %d then %d", first, again)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"type\":\"summary\"}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if grown := jsonlMarker(path, nil); grown == first {
		t.Fatalf("jsonlMarker() unchanged after append: %d", grown)
	}

	if missing := jsonlMarker(filepath.Join(t.TempDir(), "gone.jsonl"), nil); missing == 0 {
		t.Fatal("jsonlMarker() = 0 for a missing file; must not compare equal to a stored 0")
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
		// nested sub-agent session: grandparent dir holds the unique
		// hash (the parent "run-0" is the same for every sub-agent
		// session under the same parent)
		"sub/abc123/run-0/session.jsonl",
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
	for _, want := range []string{"aaa", "bbb", "abc123"} {
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
		Store:     st,
		Agents:    []string{"test"},
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

	// Run again — nothing changed, so both sessions are skipped and the
	// parser is not called again. This is the point of the store: it is a
	// cache, not a mirror that gets rebuilt from source on every run.
	res, _ = ing.Sync(context.Background())
	if res.Ingested != 0 || res.Skipped != 2 {
		t.Errorf("re-sync res=%+v, want Ingested=0 Skipped=2", res)
	}
	if parseCalls != 2 {
		t.Errorf("parseCalls=%d after re-sync, want 2 (unchanged refs must not be re-parsed)", parseCalls)
	}
	storedCount, _ = st.CountSessions(context.Background(), "test")
	if storedCount != 2 {
		t.Errorf("after re-sync storedCount=%d, want 2", storedCount)
	}

	// Force ignores the skip filter.
	ing.Force = true
	res, _ = ing.Sync(context.Background())
	if res.Ingested != 2 || res.Skipped != 0 {
		t.Errorf("forced re-sync res=%+v, want Ingested=2 Skipped=0", res)
	}
	storedCount, _ = st.CountSessions(context.Background(), "test")
	if storedCount != 2 {
		t.Errorf("after forced re-sync storedCount=%d, want 2", storedCount)
	}
}

func TestIngestor_Sync_ReingestsChangedRef(t *testing.T) {
	st := openStore(t)
	src := &mockSource{
		agent: "test",
		refs:  []SessionRef{{Agent: "test", SessionID: "s1", Source: "/p1", MTime: 100}},
	}
	parseCalls := 0
	ing := &Ingestor{
		Store:     st,
		Agents:    []string{"test"},
		SourceFor: map[string]Source{"test": src},
		ParserFor: map[string]Parser{"test": func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
			parseCalls++
			return store.ParsedSession{
				Session: store.Session{Agent: "test", SessionID: ref.SessionID, CreatedAt: 1, LastActivity: 2},
				Messages: []store.ParsedMessage{
					{Message: store.Message{Agent: "test", SessionID: ref.SessionID, MsgIndex: 0, Role: store.RoleUser, Content: "hi", CreatedAt: 1}},
				},
			}, nil
		}},
	}
	if _, err := ing.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	src.refs[0].MTime = 200 // source changed since the last run
	res, err := ing.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Ingested != 1 || res.Skipped != 0 {
		t.Errorf("res=%+v, want Ingested=1 Skipped=0", res)
	}
	if parseCalls != 2 {
		t.Errorf("parseCalls=%d, want 2", parseCalls)
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
