package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// claudeSource discovers Claude Code session files under ~/.claude/projects/.
// Each session is a .jsonl file. Subagent files live under
// <projectDir>/<sessionID>/subagents/*.jsonl and are listed separately.
type claudeSource struct {
	root string // expanded path
}

func NewClaudeSource(root string) Source {
	return &claudeSource{root: root}
}

func (s *claudeSource) Agent() string { return "claude" }
func (s *claudeSource) Root() string  { return s.root }

func (s *claudeSource) Discover(ctx context.Context) ([]SessionRef, error) {
	if _, err := os.Stat(s.root); err != nil {
		return nil, nil // source not present is not an error
	}
	var refs []SessionRef
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		sessionID := strings.TrimSuffix(d.Name(), ".jsonl")
		refs = append(refs, SessionRef{
			Agent:     "claude",
			SessionID: sessionID,
			Source:    path,
			MTime:     jsonlMarker(path, d),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Source < refs[j].Source })
	return refs, nil
}

// opencodeSource discovers OpenCode sessions by querying the opencode DB
// for distinct session IDs. The path is the opencode DB file.
type opencodeSource struct {
	dbPath string
}

func NewOpenCodeSource(dbPath string) Source {
	return &opencodeSource{dbPath: dbPath}
}

func (s *opencodeSource) Agent() string { return "opencode" }
func (s *opencodeSource) Root() string  { return s.dbPath }

func (s *opencodeSource) Discover(ctx context.Context) ([]SessionRef, error) {
	if _, err := os.Stat(s.dbPath); err != nil {
		return nil, nil
	}
	db, err := openSQLiteRO(s.dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// mtime = hash of MAX(part.id) WHERE type='step-finish'. We use the
	// part id (not time_created) because opencode appears to bump
	// part.time_created on later touches unrelated to the step-finish
	// event itself, which would make every poll look like every session
	// changed. part.id is monotonic per session, so it only changes when
	// a new step-finish part is actually inserted.
	//
	// part.id is TEXT (e.g. "prt_ecf46c930001…"), so we hash it with
	// FNV-1a to get a stable int64 we can compare against the integer
	// source_mtime column in the store. Sessions with no step-finish
	// parts get mtime=0.
	rows, err := db.QueryContext(ctx, `
		SELECT s.id,
		       (SELECT MAX(p.id) FROM part p
		        WHERE p.session_id = s.id
		          AND json_extract(p.data, '$.type') = 'step-finish')
		FROM session s
		ORDER BY s.time_created DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []SessionRef
	for rows.Next() {
		var id string
		var maxID sql.NullString
		if err := rows.Scan(&id, &maxID); err != nil {
			continue
		}
		var mtime int64
		if maxID.Valid && maxID.String != "" {
			mtime = hashID(maxID.String)
		}
		refs = append(refs, SessionRef{
			Agent:     "opencode",
			SessionID: id,
			Source:    s.dbPath,
			MTime:     mtime,
		})
	}
	return refs, rows.Err()
}

// hashID returns a stable 64-bit FNV-1a hash of s, masked to 63 bits so
// it's always non-negative when stored as int64. Used to convert
// opencode's TEXT part.id into a comparable int64 for source_mtime.
// Collision probability is ~negligible (2^-63) and false positives only
// cause one extra re-ingest, not data loss.
//
// We mask the sign bit because FNV-1a returns uint64 and roughly half
// the values have the high bit set, which would become negative as
// int64 — harmless for the skip filter's equality check, but it would
// store nonsense-looking negatives in source_mtime.
func hashID(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64() & 0x7FFFFFFFFFFFFFFF)
}

// piSource discovers Pi agent session files under <root>/<project>/<date>_<id>.jsonl.
// Sub-sessions live in nested session.jsonl files.
type piSource struct {
	root string
}

func NewPiSource(root string) Source {
	return &piSource{root: root}
}

func (s *piSource) Agent() string { return "pi" }
func (s *piSource) Root() string  { return s.root }

func (s *piSource) Discover(ctx context.Context) ([]SessionRef, error) {
	if _, err := os.Stat(s.root); err != nil {
		return nil, nil
	}
	var refs []SessionRef
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		// main session: <date>_<id>.jsonl        → id = <id> (uuid)
		// nested session: session.jsonl           → id = grandparent dir
		//                                          (e.g. <hash>, unique per
		//                                          sub-agent; using the
		//                                          parent dir "run-0" would
		//                                          collide across all
		//                                          sub-agent sessions)
		var id string
		if name == "session.jsonl" {
			grandparent := filepath.Dir(filepath.Dir(path))
			id = filepath.Base(grandparent)
		} else if strings.HasSuffix(name, ".jsonl") {
			base := strings.TrimSuffix(name, ".jsonl")
			parts := strings.SplitN(base, "_", 2)
			if len(parts) != 2 {
				return nil
			}
			id = parts[1]
		} else {
			return nil
		}
		refs = append(refs, SessionRef{
			Agent:     "pi",
			SessionID: id,
			Source:    path,
			MTime:     jsonlMarker(path, d),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Source < refs[j].Source })
	return refs, nil
}

func fileMTime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixMilli()
}

// dbFileMTime returns the most recent modification time across a SQLite
// database file and its WAL/SHM sidecars. OpenCode uses WAL mode: new writes
// land in the -wal file before being checkpointed into the main db, so
// watching only the main file misses updates that are still in the WAL.
func dbFileMTime(path string) int64 {
	t := fileMTime(path)
	for _, suf := range []string{"-wal", "-shm"} {
		if mt := fileMTime(path + suf); mt > t {
			t = mt
		}
	}
	return t
}

// jsonlMarker returns a per-file change marker for an append-only JSONL
// session log: an FNV-1a hash of (size, mtime-in-ns). The skip filter
// compares markers for inequality, so the value only has to change when
// the file does — it does not have to be monotonic.
//
// We hash size together with mtime rather than using either alone:
// a stale mtime (atomic-replace writers) is still caught by the size
// change, and a same-size rewrite is caught by mtime. The previous
// approach — the timestamp of the last JSONL entry — silently returned 0
// for ~half of the real files here (claude's trailing summary lines and
// pi entries carry no "timestamp" field), which made every such session
// look unchanged-at-zero and, worse, indistinguishable from each other.
//
// The DirEntry is optional; pass nil to stat the path directly. Using
// the entry from the WalkDir that found the file avoids a second stat.
func jsonlMarker(path string, d os.DirEntry) int64 {
	var info os.FileInfo
	var err error
	if d != nil {
		info, err = d.Info()
	} else {
		info, err = os.Stat(path)
	}
	if err != nil {
		// Fail open: an unreadable marker must never compare equal to
		// whatever is stored, or the session would be skipped forever.
		return hashID(fmt.Sprintf("stat-error:%d", time.Now().UnixNano()))
	}
	return hashID(fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano()))
}

// openSQLiteRO opens a sqlite database in read-only mode.
// We do NOT use _immutable=1: OpenCode uses WAL mode and writes land in the
// -wal file before being checkpointed into the main db. With _immutable=1
// SQLite skips the WAL entirely and returns stale data.
func openSQLiteRO(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
