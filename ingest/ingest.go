// Package ingest reads session data from agent sources
// (Claude, OpenCode, Pi) and writes it into a tokeneks store.
//
// Main types:
//
//	Source   — discovers session files for one agent
//	Parser   — converts a discovered session into store.ParsedSession
//	Ingestor — coordinates sources, parsers and the store
//
// Typical use:
//
//	ing := &ingest.Ingestor{Store: st, Agents: []string{"claude", "opencode", "pi"}, ...}
//	result, err := ing.Sync(ctx)
package ingest

import (
	"context"
	"fmt"
	"log"
	"tokeneks/store"
)

// SessionRef identifies one session from one source.
type SessionRef struct {
	Agent     string
	SessionID string
	Source    string // path or identifier
	MTime     int64  // ms epoch
}

// Source discovers session files for one agent.
//
// Root returns the path the Watcher should monitor for changes
// (a directory for filesystem-backed agents, a file path for sqlite-backed
// ones). Empty string means the source has nothing to watch.
type Source interface {
	Agent() string
	Discover(ctx context.Context) ([]SessionRef, error)
	Root() string
}

// Parser turns a SessionRef into a ParsedSession ready for storage.
type Parser func(ctx context.Context, ref SessionRef) (store.ParsedSession, error)

// SyncResult summarises one Sync run.
type SyncResult struct {
	Discovered int
	Ingested   int
	Skipped    int
	Errors     int
}

// Ingestor coordinates sources, parsers and the store.
type Ingestor struct {
	Store     *store.Store
	Agents    []string
	SourceFor map[string]Source
	ParserFor map[string]Parser
	Log       *log.Logger
	OnError   func(ref SessionRef, err error)
}

// Sync runs a full sync: discover all sessions and ingest them.
// Every discovered session is re-parsed and re-ingested unconditionally
// — we do not skip by source_mtime because filesystem mtimes for the
// JSONL sources are not a reliable signal (append-only writes and
// atomic-replace patterns can leave mtime stale, and any skip path
// risks missing changes). IngestSession's DELETE+INSERT is cheap
// enough that an unconditional re-ingest is faster than N round-trips
// to the store to check mtimes per session.
func (i *Ingestor) Sync(ctx context.Context) (SyncResult, error) {
	var res SyncResult
	for _, agent := range i.Agents {
		src, ok := i.SourceFor[agent]
		if !ok {
			continue
		}
		parser, ok := i.ParserFor[agent]
		if !ok {
			continue
		}
		refs, err := src.Discover(ctx)
		if err != nil {
			i.reportErr(SessionRef{Agent: agent}, fmt.Errorf("discover: %w", err))
			res.Errors++
			continue
		}
		res.Discovered += len(refs)
		for _, ref := range refs {
			ps, err := parser(ctx, ref)
			if err != nil {
				// Parser failed: keep any existing row. We never destroy
				// history on parse failure — better to show stale data
				// than to lose a session because of a transient read error
				// or a brief mid-write state.
				i.reportErr(ref, fmt.Errorf("parse: %w", err))
				res.Errors++
				continue
			}
			ps.Session.SourceMTime = ref.MTime
			if err := i.Store.IngestSession(ctx, ps); err != nil {
				i.reportErr(ref, fmt.Errorf("store: %w", err))
				res.Errors++
				continue
			}
			res.Ingested++
		}
	}
	return res, nil
}

func (i *Ingestor) reportErr(ref SessionRef, err error) {
	if i.OnError != nil {
		i.OnError(ref, err)
		return
	}
	if i.Log != nil {
		i.Log.Printf("ingest %s/%s: %v", ref.Agent, ref.SessionID, err)
	}
}
