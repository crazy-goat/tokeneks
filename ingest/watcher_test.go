package ingest

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"tokeneks/store"

	"github.com/fsnotify/fsnotify"
)

// echoParser returns a session with one user message and the given content.
func echoParser(content string) Parser {
	return func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
		return store.ParsedSession{
			Session: store.Session{Agent: ref.Agent, SessionID: ref.SessionID, CreatedAt: 1, LastActivity: 2},
			Messages: []store.ParsedMessage{
				{Message: store.Message{Agent: ref.Agent, SessionID: ref.SessionID, MsgIndex: 0, Role: store.RoleUser, Content: content, CreatedAt: 1}},
			},
		}, nil
	}
}

// emptyParser returns a session with no messages. Used to verify that
// empty sessions are tracked in the store (so the mtime filter can
// short-circuit subsequent polls) but do not emit SessionEvents.
func emptyParser() Parser {
	return func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
		return store.ParsedSession{
			Session: store.Session{Agent: ref.Agent, SessionID: ref.SessionID, CreatedAt: 1, LastActivity: 1},
		}, nil
	}
}

// collectEvents reads up to n events with a timeout.
func collectEvents(ch <-chan SessionEvent, n int, timeout time.Duration) []SessionEvent {
	var out []SessionEvent
	t := time.NewTimer(timeout)
	defer t.Stop()
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-t.C:
			return out
		}
	}
	return out
}

func TestWatcher_InitialSync_IngestsAndEmits(t *testing.T) {
	st := openStore(t)
	dir := t.TempDir()
	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("s1.jsonl", "{}")
	writeFile("s2.jsonl", "{}")

	src := &fileSource{agent: "claude", root: dir}
	w := NewWatcher(st, map[string]Source{src.Agent(): src}, map[string]Parser{"claude": echoParser("hi")}, WatcherConfig{
		Debounce: 50 * time.Millisecond,
	})
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// wait for events
	events := collectEvents(w.Events(), 2, 3*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	for _, ev := range events {
		if ev.Kind != Changed {
			t.Errorf("event kind = %v, want Changed", ev.Kind)
		}
	}
	n, _ := st.CountSessions(context.Background(), "claude")
	if n != 2 {
		t.Errorf("sessions in store = %d, want 2", n)
	}
}

func TestWatcher_EmptySession_StoredButNoEvent(t *testing.T) {
	st := openStore(t)
	src := &mockSource{agent: "oc", refs: []SessionRef{
		{Agent: "oc", SessionID: "empty-1", Source: "/db"},
		{Agent: "oc", SessionID: "full-1", Source: "/db"},
		{Agent: "oc", SessionID: "empty-2", Source: "/db"},
	}}
	// One parser that returns empty for empty-* ids, one real message for full-*.
	parser := func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
		if ref.SessionID == "full-1" {
			return echoParser("hi")(ctx, ref)
		}
		return emptyParser()(ctx, ref)
	}
	w := NewWatcher(st, map[string]Source{"oc": src}, map[string]Parser{"oc": parser}, WatcherConfig{})

	ctx := context.Background()
	// Re-ingest two empty refs and one non-empty ref directly. Empty
	// sessions must land in the store (so the mtime filter has a
	// baseline) but must not emit events; the non-empty one must emit.
	w.reingestRef(ctx, SessionRef{Agent: "oc", SessionID: "empty-1", Source: "/db", MTime: 0})
	w.reingestRef(ctx, SessionRef{Agent: "oc", SessionID: "empty-2", Source: "/db", MTime: 0})
	w.reingestRef(ctx, SessionRef{Agent: "oc", SessionID: "full-1", Source: "/db", MTime: 12345})

	n, _ := st.CountSessions(ctx, "oc")
	if n != 3 {
		t.Errorf("CountSessions = %d, want 3 (empty sessions must still be in the store)", n)
	}

	// Drain events: only the non-empty session should have produced one.
	ev := collectEvents(w.Events(), 1, 200*time.Millisecond)
	if len(ev) != 1 {
		t.Fatalf("got %d events, want 1 (only the non-empty session should emit)", len(ev))
	}
	if ev[0].SessionID != "full-1" {
		t.Errorf("event for %q, want %q", ev[0].SessionID, "full-1")
	}
	// No more events should arrive.
	if extra := collectEvents(w.Events(), 1, 100*time.Millisecond); len(extra) != 0 {
		t.Errorf("got %d extra events, want 0", len(extra))
	}

	// source_mtime for empty sessions is 0; for the non-empty one it's
	// the ref.MTime we passed.
	for _, c := range []struct {
		id    string
		mTime int64
	}{
		{"empty-1", 0}, {"empty-2", 0}, {"full-1", 12345},
	} {
		sess, err := st.GetSession(ctx, "oc", c.id)
		if err != nil {
			t.Fatalf("GetSession(%s): %v", c.id, err)
		}
		if sess.SourceMTime != c.mTime {
			t.Errorf("session %s SourceMTime = %d, want %d", c.id, sess.SourceMTime, c.mTime)
		}
	}
}

// The store is a cache: a session whose source has not changed since the
// last run must not be re-parsed on startup. Re-parsing every session is
// what made startup take ~25s over a few hundred MB of JSONL.
func TestWatcher_InitialSync_SkipsUnchangedSession(t *testing.T) {
	st, dir, _ := seedStoredSession(t, "stale-marker-matches")

	var parseCount int32
	w := NewWatcher(st, map[string]Source{"claude": &fileSource{agent: "claude", root: dir}}, map[string]Parser{
		"claude": func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
			atomic.AddInt32(&parseCount, 1)
			return echoParser("fresh")(ctx, ref)
		},
	}, WatcherConfig{Debounce: 20 * time.Millisecond})
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	if events := collectEvents(w.Events(), 1, 500*time.Millisecond); len(events) != 0 {
		t.Fatalf("initial events = %d, want 0 (nothing changed)", len(events))
	}
	if n := atomic.LoadInt32(&parseCount); n != 0 {
		t.Fatalf("parseCount = %d, want 0 (unchanged source must not be re-parsed)", n)
	}
}

func TestWatcher_InitialSync_ReingestsChangedSession(t *testing.T) {
	st, dir, fp := seedStoredSession(t, "stale-marker-matches")

	// Move the file's mtime forward so its marker no longer matches what's
	// stored. Chtimes rather than a rewrite: a rewrite can land in the same
	// millisecond fileSource rounds to, which would make this flaky.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(fp, future, future); err != nil {
		t.Fatal(err)
	}

	var parseCount int32
	w := NewWatcher(st, map[string]Source{"claude": &fileSource{agent: "claude", root: dir}}, map[string]Parser{
		"claude": func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
			atomic.AddInt32(&parseCount, 1)
			return echoParser("fresh")(ctx, ref)
		},
	}, WatcherConfig{Debounce: 20 * time.Millisecond})
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	if events := collectEvents(w.Events(), 1, time.Second); len(events) != 1 {
		t.Fatalf("initial events = %d, want 1", len(events))
	}
	if atomic.LoadInt32(&parseCount) != 1 {
		t.Fatalf("parseCount = %d, want 1", parseCount)
	}
	messages, err := st.GetMessages(context.Background(), "claude", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Content != "fresh" {
		t.Fatalf("messages = %+v, want fresh message", messages)
	}
}

// seedStoredSession writes dir/s1.jsonl and stores a matching session row
// whose source_mtime is the marker fileSource will compute for that file
// — i.e. the state after a previous run ingested it.
func seedStoredSession(t *testing.T, content string) (st *store.Store, dir, path string) {
	t.Helper()
	st = openStore(t)
	dir = t.TempDir()
	path = filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.IngestSession(context.Background(), store.ParsedSession{
		Session:  store.Session{Agent: "claude", SessionID: "s1", CreatedAt: 1, LastActivity: 2, SourceMTime: info.ModTime().UnixMilli()},
		Messages: []store.ParsedMessage{{Message: store.Message{Agent: "claude", SessionID: "s1", MsgIndex: 0, Role: store.RoleUser, Content: content, CreatedAt: 1}}},
	}); err != nil {
		t.Fatal(err)
	}
	return st, dir, path
}

func TestWatcher_FileChange_Reingests(t *testing.T) {
	st := openStore(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(fp, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &fileSource{agent: "claude", root: dir}
	var parseCount int32
	counterParser := func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
		atomic.AddInt32(&parseCount, 1)
		return echoParser("hi")(ctx, ref)
	}
	w := NewWatcher(st, map[string]Source{src.Agent(): src}, map[string]Parser{"claude": counterParser}, WatcherConfig{
		Debounce: 50 * time.Millisecond,
	})
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// initial event
	collectEvents(w.Events(), 1, 2*time.Second)

	// modify the file
	if err := os.WriteFile(fp, []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	// wait for re-ingest event
	collectEvents(w.Events(), 1, 2*time.Second)

	if atomic.LoadInt32(&parseCount) < 2 {
		t.Errorf("parseCount=%d, want >=2 (initial + modify)", parseCount)
	}
}

func TestWatcher_Rename_Reingests(t *testing.T) {
	st := openStore(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(fp, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var parseCount int32
	w := NewWatcher(st, map[string]Source{"claude": &fileSource{agent: "claude", root: dir}}, map[string]Parser{
		"claude": func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
			atomic.AddInt32(&parseCount, 1)
			return echoParser("renamed")(ctx, ref)
		},
	}, WatcherConfig{Debounce: 20 * time.Millisecond})
	defer w.Close()

	w.handleFSEvent(context.Background(), fsnotify.Event{Name: fp, Op: fsnotify.Rename})
	events := collectEvents(w.Events(), 1, time.Second)
	if len(events) != 1 || events[0].SessionID != "s1" || events[0].Kind != Changed {
		t.Fatalf("events = %+v, want one changed s1 event", events)
	}
	if atomic.LoadInt32(&parseCount) != 1 {
		t.Fatalf("parseCount = %d, want 1", parseCount)
	}
}

func TestWatcher_FileRemove_KeepsSessionEmitsEvent(t *testing.T) {
	st := openStore(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(fp, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &fileSource{agent: "claude", root: dir}
	w := NewWatcher(st, map[string]Source{src.Agent(): src}, map[string]Parser{"claude": echoParser("hi")}, WatcherConfig{
		Debounce: 50 * time.Millisecond,
	})
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	collectEvents(w.Events(), 1, 2*time.Second) // initial

	n, _ := st.CountSessions(context.Background(), "claude")
	if n != 1 {
		t.Fatalf("after init: sessions=%d, want 1", n)
	}

	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	// wait for remove event
	ev := collectEvents(w.Events(), 1, 2*time.Second)
	if len(ev) != 1 {
		t.Fatalf("got %d events after remove, want 1", len(ev))
	}
	if ev[0].Kind != Removed {
		t.Errorf("event kind = %v, want Removed", ev[0].Kind)
	}
	// Session is intentionally kept in the store so history is not lost.
	n, _ = st.CountSessions(context.Background(), "claude")
	if n != 1 {
		t.Errorf("after remove: sessions=%d, want 1 (session kept for history)", n)
	}
}

func TestWatcher_Debounce_CoalescesEvents(t *testing.T) {
	st := openStore(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(fp, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &fileSource{agent: "claude", root: dir}
	var parseCount int32
	w := NewWatcher(st, map[string]Source{src.Agent(): src},
		map[string]Parser{"claude": func(ctx context.Context, ref SessionRef) (store.ParsedSession, error) {
			atomic.AddInt32(&parseCount, 1)
			return echoParser("x")(ctx, ref)
		}}, WatcherConfig{Debounce: 200 * time.Millisecond})
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	collectEvents(w.Events(), 1, 2*time.Second) // initial

	// hammer the file
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(fp, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	collectEvents(w.Events(), 1, 2*time.Second) // one coalesced event

	// Expect: initial + 1 debounced batch. parseCount should be small.
	got := atomic.LoadInt32(&parseCount)
	if got > 5 {
		t.Errorf("parseCount=%d, debounce not working (expected <=5)", got)
	}
}

func TestResolveSessionID(t *testing.T) {
	cases := []struct {
		path    string
		wantAgt string
		wantID  string
	}{
		{"/x/sess-abc.jsonl", "claude", "sess-abc"},
		// pi main session: actual filename format is
		// <timestamp-with-T-and-millis>_<uuid>.jsonl
		{"/x/2026-06-15T08-30-24-854Z_aaa.jsonl", "pi", "aaa"},
		// pi main session: shorter date-only prefix also works
		{"/x/2026-06-15_aaa.jsonl", "pi", "aaa"},
		// pi nested session.jsonl: id is the GRANDPARENT dir, not the
		// parent — using parent would collide on "run-0" across all
		// sub-agent sessions sharing a parent
		{"/x/sub/session.jsonl", "pi", "x"},
		// pi real-world shape: <proj>/<date>_<id>/<hash>/run-0/session.jsonl
		{"/x/proj/2026-06-15_aaa/abc123/run-0/session.jsonl", "pi", "abc123"},
		{"/x/random.txt", "", ""},
	}
	for _, c := range cases {
		agt, id := resolveSessionID(c.path)
		if agt != c.wantAgt || id != c.wantID {
			t.Errorf("resolveSessionID(%q) = (%q,%q), want (%q,%q)", c.path, agt, id, c.wantAgt, c.wantID)
		}
	}
}

// fileSource is a minimal Source backed by a directory of *.jsonl files.
// Each file is its own session whose ID is the filename minus .jsonl.
type fileSource struct {
	agent string
	root  string
}

func (f *fileSource) Agent() string { return f.agent }
func (f *fileSource) Root() string  { return f.root }
func (f *fileSource) Discover(ctx context.Context) ([]SessionRef, error) {
	entries, err := os.ReadDir(f.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var refs []SessionRef
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, _ := e.Info()
		var mtime int64
		if info != nil {
			mtime = info.ModTime().UnixMilli()
		}
		refs = append(refs, SessionRef{
			Agent:     f.agent,
			SessionID: e.Name()[:len(e.Name())-len(".jsonl")],
			Source:    filepath.Join(f.root, e.Name()),
			MTime:     mtime,
		})
	}
	return refs, nil
}
