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
	// Force, if true, re-ingests every session regardless of source_mtime.
	Force bool
}

// Sync runs a full sync: discover all sessions and ingest them.
// Sessions whose source_mtime in the store is >= the discovered mtime
// are skipped (incremental). Set Ingestor.Force to re-ingest everything.
// Sessions that the parser can't read (or that come back with no messages)
// cause any existing row in the store to be removed — they are stale.
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
			if !i.Force && ref.MTime > 0 {
				existing, err := i.Store.GetSession(ctx, ref.Agent, ref.SessionID)
				if err == nil && existing.SourceMTime >= ref.MTime {
					// Stale empty rows (e.g. session whose source no longer has
					// step-finish parts) are always re-parsed so they get cleaned
					// up by the parser-failure path below.
					if !i.sessionIsEmpty(ctx, ref) {
						res.Skipped++
						continue
					}
				}
			}
			ps, err := parser(ctx, ref)
			if err != nil {
				i.reportErr(ref, fmt.Errorf("parse: %w", err))
				res.Errors++
				// Parser failed: drop any stale row so the web doesn't show empty sessions.
				if delErr := i.Store.DeleteSession(ctx, ref.Agent, ref.SessionID); delErr != nil {
					i.reportErr(ref, fmt.Errorf("cleanup: %w", delErr))
				}
				continue
			}
			if len(ps.Messages) == 0 {
				// Nothing to store: drop any stale row.
				if err := i.Store.DeleteSession(ctx, ref.Agent, ref.SessionID); err != nil {
					i.reportErr(ref, fmt.Errorf("cleanup: %w", err))
				}
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

// sessionIsEmpty returns true if the session in the store has no messages.
// Used to detect stale empty rows that should be re-parsed and cleaned up.
func (i *Ingestor) sessionIsEmpty(ctx context.Context, ref SessionRef) bool {
	msgs, err := i.Store.GetMessages(ctx, ref.Agent, ref.SessionID)
	if err != nil {
		return false
	}
	return len(msgs) == 0
}
