package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func expandHome(path string) string {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func parseTimestamp(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02 15:04:05", value)
}

var (
	ocDBMu  sync.Mutex
	ocDB    *sql.DB
	ocDBErr error
)

func openOCDB() (*sql.DB, error) {
	ocDBMu.Lock()
	defer ocDBMu.Unlock()
	if ocDB != nil || ocDBErr != nil {
		return ocDB, ocDBErr
	}
	ocDB, ocDBErr = sql.Open("sqlite3", expandHome(defaultDB))
	return ocDB, ocDBErr
}

// piSessionIDFromFilename extracts the session ID from a PI session filename.
// Filename format: <date>_<sessionID>.jsonl (e.g., "2025-01-15_abc123.jsonl").
// Returns an error if the name is too short or has no underscore.
func piSessionIDFromFilename(name string) (string, error) {
	base := strings.TrimSuffix(name, ".jsonl")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid filename format: %q", name)
	}
	if len(parts[0]) < 10 {
		return "", fmt.Errorf("filename too short for date prefix: %q", name)
	}
	return parts[1], nil
}

// fileDateFromFilename extracts the date prefix (first 10 chars) from a filename.
// Strips .jsonl extension first. Returns false if the base name is too short.
func fileDateFromFilename(name string) (string, bool) {
	base := strings.TrimSuffix(name, ".jsonl")
	if len(base) < 10 {
		return "", false
	}
	return base[:10], true
}

func formatTokens(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
