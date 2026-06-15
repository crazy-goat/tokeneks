package ingest

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"tokeneks/store"
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
	go w.Run(ctx)

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
	go w.Run(ctx)

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

func TestWatcher_FileRemove_DeletesFromStore(t *testing.T) {
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
	go w.Run(ctx)
	collectEvents(w.Events(), 1, 2*time.Second) // initial

	n, _ := st.CountSessions(context.Background(), "claude")
	if n != 1 {
		t.Fatalf("after init: sessions=%d, want 1", n)
	}

	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	// wait for remove event
	ev := <-w.Events()
	if ev.Kind != Removed {
		t.Errorf("event kind = %v, want Removed", ev.Kind)
	}
	n, _ = st.CountSessions(context.Background(), "claude")
	if n != 0 {
		t.Errorf("after remove: sessions=%d, want 0", n)
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
	go w.Run(ctx)
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
		path     string
		wantAgt  string
		wantID   string
	}{
		{"/x/sess-abc.jsonl", "claude", "sess-abc"},
		{"/x/2026-06-15_aaa.jsonl", "pi", "aaa"},
		{"/x/sub/session.jsonl", "pi", "sub"},
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
