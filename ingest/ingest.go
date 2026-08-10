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
	Store      *store.Store
	Agents     []string
	SourceFor  map[string]Source
	ParserFor  map[string]Parser
	Log        *log.Logger
	OnError    func(ref SessionRef, err error)
	OnProgress func(agent string, current, total int)
	// Force re-parses every discovered session even if its change
	// marker matches the store. Off by default: the store is the cache,
	// and re-reading hundreds of MB of JSONL on every start is the
	// single most expensive thing this tool does.
	Force bool
}

// Sync discovers all sessions and ingests the ones whose source changed
// since the last run (see FilterChangedRefs). Sessions already in the
// store with an unchanged marker are counted as Skipped and never
// re-parsed — that is what makes the sqlite store an actual cache
// rather than a write-through mirror. Set Force to re-ingest everything.
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
		if !i.Force {
			changed, err := i.FilterChangedRefs(ctx, agent, refs)
			if err != nil {
				i.reportErr(SessionRef{Agent: agent}, fmt.Errorf("mtime filter: %w", err))
			}
			res.Skipped += len(refs) - len(changed)
			refs = changed
		}
		for idx, ref := range refs {
			ps, err := parser(ctx, ref)
			if err != nil {
				// Parser failed: keep any existing row. We never destroy
				// history on parse failure — better to show stale data
				// than to lose a session because of a transient read error
				// or a brief mid-write state.
				i.reportErr(ref, fmt.Errorf("parse: %w", err))
				res.Errors++
				if i.OnProgress != nil {
					i.OnProgress(agent, idx+1, len(refs))
				}
				continue
			}
			ps.Session.SourceMTime = ref.MTime
			if err := i.Store.IngestSession(ctx, ps); err != nil {
				i.reportErr(ref, fmt.Errorf("store: %w", err))
				res.Errors++
				if i.OnProgress != nil {
					i.OnProgress(agent, idx+1, len(refs))
				}
				continue
			}
			res.Ingested++
			if i.OnProgress != nil {
				i.OnProgress(agent, idx+1, len(refs))
			}
		}
	}
	return res, nil
}

// FilterChangedRefs drops refs whose per-session change marker equals
// what's already in the store. One batch query per agent (not N
// per-session queries). Marker sources per agent:
//   - opencode: hash of MAX(part.id) WHERE type='step-finish' — only
//     changes when a new step-finish part is actually inserted
//   - claude, pi: hash of (file size, file mtime) — see jsonlMarker
//
// Sessions not in the store yet are always ingested. Returns refs as-is
// on lookup failure (fail open: re-ingest everything).
func (i *Ingestor) FilterChangedRefs(ctx context.Context, agent string, refs []SessionRef) ([]SessionRef, error) {
	if len(refs) == 0 {
		return refs, nil
	}
	stored, err := i.Store.GetSessionMTimes(ctx, agent)
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
	return changed, nil
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
