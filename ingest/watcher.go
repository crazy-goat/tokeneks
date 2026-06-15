package ingest

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"tokeneks/store"
)

// ChangeKind describes how a session's underlying source changed.
type ChangeKind int

const (
	// Changed means the source file was modified or created.
	// The watcher will re-parse and re-ingest.
	Changed ChangeKind = iota
	// Removed means the source file was deleted.
	// The session is deleted from the store.
	Removed
)

func (k ChangeKind) String() string {
	switch k {
	case Changed:
		return "changed"
	case Removed:
		return "removed"
	}
	return "unknown"
}

// SessionEvent is emitted by the Watcher whenever a session's
// underlying source file changes.
type SessionEvent struct {
	Agent     string
	SessionID string
	Source    string
	Kind      ChangeKind
	Time      time.Time
}

// WatcherConfig tunes Watcher behavior.
type WatcherConfig struct {
	// Debounce coalesces multiple fsnotify events for the same path
	// within this window. Default 300ms.
	Debounce time.Duration
	// OpenCodePoll is how often the OpenCode source's sqlite file is
	// checked for mtime changes. Default 5s.
	OpenCodePoll time.Duration
	// Logger; nil = silent.
	Logger *log.Logger
}

// Watcher monitors agent sources for changes and re-ingests
// affected sessions. Use Events() to observe changes.
type Watcher struct {
	store    *store.Store
	ingestor *Ingestor
	cfg      WatcherConfig
	log      *log.Logger

	events chan SessionEvent

	mu      sync.Mutex
	fsw     *fsnotify.Watcher
	pending map[string]*time.Timer
}

// NewWatcher returns a Watcher that re-ingests sessions when their
// underlying source files change.
func NewWatcher(st *store.Store, sources map[string]Source, parsers map[string]Parser, cfg WatcherConfig) *Watcher {
	if cfg.Debounce == 0 {
		cfg.Debounce = 300 * time.Millisecond
	}
	if cfg.OpenCodePoll == 0 {
		cfg.OpenCodePoll = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "[watch] ", log.LstdFlags)
	}
	return &Watcher{
		store: st,
		ingestor: &Ingestor{
			Store:     st,
			SourceFor: sources,
			ParserFor: parsers,
			Log:       cfg.Logger,
		},
		cfg:     cfg,
		log:     cfg.Logger,
		events:  make(chan SessionEvent, 128),
		pending: make(map[string]*time.Timer),
	}
}

// Events returns a read-only channel of session change events.
// The channel is closed when the Watcher is closed.
func (w *Watcher) Events() <-chan SessionEvent { return w.events }

// Run starts watching. It blocks until ctx is cancelled.
// On startup, every currently-discovered session is re-ingested if its
// source changed (incremental via Ingestor.Sync); unchanged sessions are
// skipped.
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw
	defer fsw.Close()

	// initial sync (incremental): the Ingestor skips up-to-date sessions.
	for agent, src := range w.ingestor.SourceFor {
		refs, err := src.Discover(ctx)
		if err != nil {
			w.log.Printf("initial discover %s: %v", agent, err)
			continue
		}
		for _, ref := range refs {
			if !w.shouldSkip(ctx, ref) {
				w.reingest(ctx, ref)
			}
		}
		if root := src.Root(); root != "" && dirExists(root) {
			if err := w.addRecursive(root); err != nil {
				w.log.Printf("watch %s: %v", root, err)
			}
		}
	}

	// OpenCode polling
	if oc, ok := w.ingestor.SourceFor["opencode"]; ok {
		go w.pollOpenCode(ctx, oc)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			w.handleFSEvent(ctx, ev)
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.log.Printf("fsnotify: %v", err)
		}
	}
}

func (w *Watcher) shouldSkip(ctx context.Context, ref SessionRef) bool {
	if ref.MTime == 0 {
		return false
	}
	existing, err := w.store.GetSession(ctx, ref.Agent, ref.SessionID)
	if err != nil {
		return false
	}
	return existing.SourceMTime >= ref.MTime
}

func (w *Watcher) addRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if err := w.fsw.Add(path); err != nil {
				w.log.Printf("watch %s: %v", path, err)
			}
		}
		return nil
	})
}

func (w *Watcher) handleFSEvent(ctx context.Context, ev fsnotify.Event) {
	if !strings.HasSuffix(ev.Name, ".jsonl") {
		return
	}
	if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Remove) {
		return
	}
	kind := Changed
	if ev.Has(fsnotify.Remove) {
		kind = Removed
	}

	w.mu.Lock()
	if t, ok := w.pending[ev.Name]; ok {
		t.Stop()
	}
	w.pending[ev.Name] = time.AfterFunc(w.cfg.Debounce, func() {
		w.mu.Lock()
		delete(w.pending, ev.Name)
		w.mu.Unlock()
		w.processChange(ctx, ev.Name, kind)
	})
	w.mu.Unlock()
}

func (w *Watcher) processChange(ctx context.Context, path string, kind ChangeKind) {
	agent, sessionID := resolveSessionID(path)
	if sessionID == "" {
		return
	}
	ref := SessionRef{Agent: agent, SessionID: sessionID, Source: path, MTime: nowMs()}

	if kind == Removed {
		if err := w.store.DeleteSession(ctx, agent, sessionID); err != nil {
			w.log.Printf("delete %s/%s: %v", agent, sessionID, err)
			return
		}
		w.emit(SessionEvent{Agent: agent, SessionID: sessionID, Source: path, Kind: Removed, Time: time.Now()})
		return
	}
	w.reingestRef(ctx, ref)
}

func (w *Watcher) reingest(ctx context.Context, ref SessionRef) {
	w.reingestRef(ctx, ref)
}

func (w *Watcher) reingestRef(ctx context.Context, ref SessionRef) {
	parser, ok := w.ingestor.ParserFor[ref.Agent]
	if !ok {
		return
	}
	ps, err := parser(ctx, ref)
	if err != nil {
		w.log.Printf("parse %s/%s: %v", ref.Agent, ref.SessionID, err)
		return
	}
	if err := w.store.IngestSession(ctx, ps); err != nil {
		w.log.Printf("ingest %s/%s: %v", ref.Agent, ref.SessionID, err)
		return
	}
	w.emit(SessionEvent{Agent: ref.Agent, SessionID: ref.SessionID, Source: ref.Source, Kind: Changed, Time: time.Now()})
}

func (w *Watcher) pollOpenCode(ctx context.Context, src Source) {
	var lastMTime int64
	seen := make(map[string]bool)
	t := time.NewTicker(w.cfg.OpenCodePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			root := src.Root()
			if root == "" {
				continue
			}
			info, err := os.Stat(root)
			if err != nil {
				continue
			}
			mtime := info.ModTime().UnixMilli()
			if mtime == lastMTime {
				continue
			}
			lastMTime = mtime
			refs, err := src.Discover(ctx)
			if err != nil {
				w.log.Printf("opencode re-discover: %v", err)
				continue
			}
			for _, ref := range refs {
				// Always re-ingest on DB mtime change — simpler and idempotent.
				_ = seen
				w.reingestRef(ctx, ref)
			}
		}
	}
}

func (w *Watcher) emit(ev SessionEvent) {
	select {
	case w.events <- ev:
	default:
		w.log.Printf("event channel full, dropping %s/%s", ev.Agent, ev.SessionID)
	}
}

// Close stops the watcher and releases resources.
func (w *Watcher) Close() error {
	if w.fsw != nil {
		err := w.fsw.Close()
		w.fsw = nil
		return err
	}
	return nil
}

// resolveSessionID returns the (agent, sessionID) for a changed file path.
// Heuristic: top-level *.jsonl → <id>, pi-style <date>_<id>.jsonl → <id>,
// pi nested session.jsonl → parent dir name.
func resolveSessionID(path string) (agent, id string) {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".jsonl") {
		return "", ""
	}
	if base == "session.jsonl" {
		// pi nested session
		return "pi", filepath.Base(filepath.Dir(path))
	}
	stem := strings.TrimSuffix(base, ".jsonl")
	if parts := strings.SplitN(stem, "_", 2); len(parts) == 2 {
		if _, err := time.Parse("2006-01-02", parts[0]); err == nil {
			return "pi", parts[1]
		}
	}
	return "claude", stem
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func nowMs() int64 { return time.Now().UnixMilli() }
