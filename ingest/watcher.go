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

	// mu protects fsw and closed. fsw is read by addRecursive and
	// written by Run / Close; closed is set to true once Close has
	// been called so addRecursive can stop calling fsw.Add on a
	// closed fsnotify.Watcher (which would panic inside fsnotify).
	mu      sync.Mutex
	fsw     *fsnotify.Watcher
	closed  bool
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
// On startup, every currently-discovered session is re-ingested
// unconditionally — see ingest.Ingestor.Sync for the rationale
// (filesystem mtimes are not a reliable signal for JSONL sources, and
// a N-query skip check is slower than just re-parsing).
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.mu.Lock()
	if w.closed {
		// Close was called before Run started. Don't install fsw,
		// and clean up the one we just created.
		w.mu.Unlock()
		_ = fsw.Close()
		return nil
	}
	w.fsw = fsw
	w.mu.Unlock()
	defer fsw.Close()

	// Set up filesystem watches before the initial ingest so that any
	// changes arriving while we are parsing are caught by fsnotify and
	// re-ingested via the debounce path. If we watched after ingesting,
	// a file modified between emit and addRecursive would be silently lost.
	for _, src := range w.ingestor.SourceFor {
		if root := src.Root(); root != "" && dirExists(root) {
			if err := w.addRecursive(root); err != nil {
				w.log.Printf("watch %s: %v", root, err)
			}
		}
	}

	// initial sync
	start := time.Now()
	for agent, src := range w.ingestor.SourceFor {
		refs, err := src.Discover(ctx)
		if err != nil {
			w.log.Printf("initial discover %s: %v", agent, err)
			continue
		}
		refs, err = w.filterChangedRefs(ctx, agent, refs)
		if err != nil {
			w.log.Printf("filter %s: %v", agent, err)
			// continue with original refs (fail open)
		}
		for _, ref := range refs {
			w.reingest(ctx, ref)
		}
	}
	w.log.Printf("initial sync done in %s", time.Since(start))

	// OpenCode: watch the DB directory for WAL/db changes.
	if oc, ok := w.ingestor.SourceFor["opencode"]; ok {
		go w.watchOpenCode(ctx, oc)
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

// filterChangedRefs drops refs whose per-session change marker is at or
// below what's already in the store. One batch query per agent (not N
// per-session queries). MTime sources per agent:
//   - opencode: MAX(part.id) WHERE type='step-finish' — monotonic,
//     never bumps on unrelated touches
//   - claude, pi: timestamp of the last JSONL entry in the file —
//     stable for append-only logs
// Sessions not in the store yet are always ingested. Returns refs as-is
// on lookup failure (fail open: re-ingest everything).
func (w *Watcher) filterChangedRefs(ctx context.Context, agent string, refs []SessionRef) ([]SessionRef, error) {
	if len(refs) == 0 {
		return refs, nil
	}
	stored, err := w.store.GetSessionMTimes(ctx, agent)
	if err != nil {
		return refs, err
	}
	changed := refs[:0:0] // new slice, no aliasing
	for _, ref := range refs {
		existing, ok := stored[ref.SessionID]
		if !ok || existing != ref.MTime {
			changed = append(changed, ref)
		}
	}
	if w.log != nil && len(refs) != len(changed) {
		w.log.Printf("%s skip: %d unchanged, %d changed", agent, len(refs)-len(changed), len(changed))
	}
	return changed, nil
}

func (w *Watcher) addRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Snapshot fsw under the lock and bail out if the
			// watcher has been closed — calling Add on a closed
			// fsnotify.Watcher panics inside the library.
			w.mu.Lock()
			fsw := w.fsw
			closed := w.closed
			w.mu.Unlock()
			if closed || fsw == nil {
				return filepath.SkipAll
			}
			if err := fsw.Add(path); err != nil {
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

	if kind == Removed {
		// Source file deleted: keep the store row so the user retains
		// history. Emit a "Removed" event so listeners (cache invalidator
		// etc.) can react, but never wipe the store.
		w.emit(SessionEvent{Agent: agent, SessionID: sessionID, Source: path, Kind: Removed, Time: time.Now()})
		return
	}
	ref := SessionRef{Agent: agent, SessionID: sessionID, Source: path, MTime: nowMs()}
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
	// Carry the per-session change marker through to the store. The
	// parser doesn't know it (it works on file contents only), so we
	// overwrite after parsing. Without this the store's source_mtime
	// would always be 0 and the skip filter would never work.
	ps.Session.SourceMTime = ref.MTime

	// Empty sessions (e.g. an opencode session that was just opened
	// and has no step-finish parts yet) are still inserted into the
	// store so the mtime filter has a baseline to skip them on
	// subsequent polls. We deliberately do NOT emit a SessionEvent
	// for them: the web list hides empty sessions (EXISTS in
	// web_store.go), so cache invalidation and SSE updates for them
	// would be wasted work. The session will start emitting events as
	// soon as messages show up.
	if len(ps.Messages) == 0 {
		_ = w.store.IngestSession(ctx, ps)
		return
	}

	if err := w.store.IngestSession(ctx, ps); err != nil {
		w.log.Printf("ingest %s/%s: %v", ref.Agent, ref.SessionID, err)
		return
	}
	w.emit(SessionEvent{Agent: ref.Agent, SessionID: ref.SessionID, Source: ref.Source, Kind: Changed, Time: time.Now()})
}

// watchOpenCode watches the directory that contains the opencode.db file
// using fsnotify and re-ingests changed sessions whenever the main db or
// its WAL sidecar is written. Falls back to a ticker if fsnotify cannot
// be set up.
func (w *Watcher) watchOpenCode(ctx context.Context, src Source) {
	root := src.Root()
	if root == "" {
		return
	}
	dbDir := filepath.Dir(root)
	dbName := filepath.Base(root)

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.log.Printf("opencode: fsnotify unavailable (%v); falling back to %s poll", err, w.cfg.OpenCodePoll)
		w.pollOpenCodeByTicker(ctx, src)
		return
	}
	defer fsw.Close()

	if err := fsw.Add(dbDir); err != nil {
		w.log.Printf("opencode: cannot watch %s (%v); falling back to poll", dbDir, err)
		_ = fsw.Close()
		w.pollOpenCodeByTicker(ctx, src)
		return
	}
	w.log.Printf("opencode: watching %s", dbDir)

	var (
		mu      sync.Mutex
		pending *time.Timer
	)
	debounce := func() {
		mu.Lock()
		if pending != nil {
			pending.Stop()
		}
		pending = time.AfterFunc(w.cfg.Debounce, func() {
			mu.Lock()
			pending = nil
			mu.Unlock()
			w.rediscoverOpenCode(ctx, src)
		})
		mu.Unlock()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-fsw.Events:
			if !ok {
				return
			}
			base := filepath.Base(ev.Name)
			if base != dbName && base != dbName+"-wal" {
				continue
			}
			if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Rename) {
				continue
			}
			debounce()
		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			w.log.Printf("opencode fsnotify: %v", err)
		}
	}
}

// pollOpenCodeByTicker is the fallback when fsnotify cannot watch the
// opencode DB directory. It checks for changes every OpenCodePoll interval.
func (w *Watcher) pollOpenCodeByTicker(ctx context.Context, src Source) {
	t := time.NewTicker(w.cfg.OpenCodePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.rediscoverOpenCode(ctx, src)
		}
	}
}

func (w *Watcher) rediscoverOpenCode(ctx context.Context, src Source) {
	refs, err := src.Discover(ctx)
	if err != nil {
		w.log.Printf("opencode re-discover: %v", err)
		return
	}
	refs, err = w.filterChangedRefs(ctx, "opencode", refs)
	if err != nil {
		w.log.Printf("opencode filter: %v", err)
	}
	for _, ref := range refs {
		w.reingestRef(ctx, ref)
	}
}

func (w *Watcher) emit(ev SessionEvent) {
	select {
	case w.events <- ev:
	default:
		w.log.Printf("event channel full, dropping %s/%s", ev.Agent, ev.SessionID)
	}
}

// Close stops the watcher and releases resources. Safe to call
// concurrently with Run: the closed flag is checked under the same
// mutex that protects fsw, so addRecursive will see the closure and
// stop calling fsw.Add on a half-closed fsnotify.Watcher.
func (w *Watcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var err error
	if w.fsw != nil {
		err = w.fsw.Close()
		w.fsw = nil
	}
	return err
}

// resolveSessionID returns the (agent, sessionID) for a changed file path.
// Must stay in sync with piSource.Discover (and piSessionIDFromPath in the
// main package) so fsnotify-triggered events carry the same id as the
// ref that the parser/ingester uses:
//   - pi session.jsonl: id = grandparent dir name (e.g. <hash>, unique
//     per sub-agent; using the parent dir "run-0" would collide)
//   - pi <date>T<time>_<id>.jsonl: id = <id> (the part after the first
//     underscore; the prefix is a pi timestamp, not a strict date)
//   - claude *.jsonl: id = filename stem
func resolveSessionID(path string) (agent, id string) {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".jsonl") {
		return "", ""
	}
	if base == "session.jsonl" {
		return "pi", filepath.Base(filepath.Dir(filepath.Dir(path)))
	}
	stem := strings.TrimSuffix(base, ".jsonl")
	if idx := strings.Index(stem, "_"); idx > 0 {
		prefix := stem[:idx]
		// pi timestamp files look like 2026-06-11T08-30-24-854Z_<id>.jsonl
		if looksLikePiTimestampPrefix(prefix) {
			return "pi", stem[idx+1:]
		}
	}
	return "claude", stem
}

// looksLikePiTimestampPrefix reports whether s starts with a YYYY-MM-DD
// date — the actual pi timestamp filenames add a "Thh-mm-ss-fffZ" tail,
// but we only check the date part to keep this heuristic cheap.
func looksLikePiTimestampPrefix(s string) bool {
	return len(s) >= 10 && s[4] == '-' && s[7] == '-' &&
		isASCIIDigit(s[0]) && isASCIIDigit(s[1]) && isASCIIDigit(s[2]) && isASCIIDigit(s[3]) &&
		isASCIIDigit(s[5]) && isASCIIDigit(s[6]) &&
		isASCIIDigit(s[8]) && isASCIIDigit(s[9])
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func nowMs() int64 { return time.Now().UnixMilli() }
