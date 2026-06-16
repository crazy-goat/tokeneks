package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
		// Per-file mtime is unreliable for JSONL append-only files
		// (claude's atomic-replace pattern can leave mtime stale), so
		// we use the timestamp of the last message in the file. Reading
		// only the tail (~4KB) keeps this O(1) regardless of file size.
		ts, _ := lastJSONLMessageTime(path)
		refs = append(refs, SessionRef{
			Agent:     "claude",
			SessionID: sessionID,
			Source:    path,
			MTime:     ts,
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
// int64 and break the `existing < ref.MTime` comparison in the skip
// filter (negative values would be treated as "smaller" than zero).
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
		// Per-file mtime is unreliable for JSONL — use the timestamp of
		// the last message in the file instead. Same approach as
		// claudeSource.
		ts, _ := lastJSONLMessageTime(path)
		refs = append(refs, SessionRef{
			Agent:     "pi",
			SessionID: id,
			Source:    path,
			MTime:     ts,
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

// lastJSONLMessageTime returns the timestamp of the last entry in a
// JSONL file, parsed as RFC3339 (with nanosecond fallback), or 0 on
// any error or empty file. The file is assumed to have one JSON object
// per line; we read only the tail (a few KB) so the cost is constant
// regardless of file size. Used as a per-file change signal that is
// more reliable than os.Stat mtime for append-only agent session logs.
func lastJSONLMessageTime(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return 0, err
	}
	const tailSize = 4096
	off := int64(0)
	if info.Size() > tailSize {
		off = info.Size() - tailSize
	}
	buf := make([]byte, info.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return 0, err
	}
	// Trim any leading partial line if we started mid-file.
	if off > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	// Find the last newline; everything after it is the last line.
	var lastLine []byte
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		lastLine = buf[i+1:]
	} else {
		lastLine = buf
	}
	lastLine = bytes.TrimSpace(lastLine)
	if len(lastLine) == 0 || lastLine[0] != '{' {
		return 0, nil
	}
	var entry struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(lastLine, &entry); err != nil || entry.Timestamp == "" {
		return 0, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.Parse(layout, entry.Timestamp); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, nil
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
