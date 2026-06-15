package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
		info, err := d.Info()
		if err != nil {
			return nil
		}
		refs = append(refs, SessionRef{
			Agent:     "claude",
			SessionID: sessionID,
			Source:    path,
			MTime:     info.ModTime().UnixMilli(),
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
	// mtime = max(session.time_created, max(part.time_created)).
	// This captures both new sessions and updates to existing ones.
	rows, err := db.QueryContext(ctx, `
		SELECT s.id,
		       ifnull(s.time_created, 0),
		       ifnull((SELECT MAX(time_created) FROM part WHERE session_id = s.id), 0)
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
		var sessionTS, partTS int64
		if err := rows.Scan(&id, &sessionTS, &partTS); err != nil {
			continue
		}
		mtime := sessionTS
		if partTS > mtime {
			mtime = partTS
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
		// main session: <date>_<id>.jsonl  → use <id>
		// nested session: session.jsonl      → use parent dir name
		var id string
		if name == "session.jsonl" {
			id = filepath.Base(filepath.Dir(path))
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
		info, err := d.Info()
		if err != nil {
			return nil
		}
		refs = append(refs, SessionRef{
			Agent:     "pi",
			SessionID: id,
			Source:    path,
			MTime:     info.ModTime().UnixMilli(),
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

// openSQLiteRO opens a sqlite database in read-only mode.
func openSQLiteRO(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_immutable=1", path)
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
