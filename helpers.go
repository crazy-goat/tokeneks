package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	scannerInitBuf          = 1024 * 1024 // 1 MB initial buffer for JSONL scanner
	scannerMaxBuf           = 10 * 1024 * 1024
	tokensPerMillion        = 1_000_000
	separatorWidthKimi      = 88
	separatorWidthClaude    = 108
	separatorWidthOpenCode  = 173
	separatorWidthPi        = 154
	separatorWidthClaudeMix = 179
)

// compactThresholdPct is the compact-display threshold in percent.
// It is a var (not const) so tests can verify the constant is used.
var compactThresholdPct = 80

// newJSONLScanner creates a bufio.Scanner with a large buffer suitable for JSONL files.
// The default 64 KB scanner limit is too small for long JSONL lines.
func newJSONLScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, scannerInitBuf), scannerMaxBuf)
	return scanner
}

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

func sessionIDFromBase(base string) (string, bool) {
	parts := strings.SplitN(base, "_", 2)
	if len(parts) != 2 {
		return "", false
	}
	return parts[1], true
}

// piSessionIDFromFilename extracts the session ID from a PI session filename.
// Filename format: <date>_<sessionID>.jsonl (e.g., "2025-01-15_abc123.jsonl").
// Returns an error if the name is too short or has no underscore.
func piSessionIDFromFilename(name string) (string, error) {
	base := strings.TrimSuffix(name, ".jsonl")
	underscore := strings.IndexByte(base, '_')
	if underscore < 10 {
		return "", fmt.Errorf("invalid filename format: %q", name)
	}
	sessionID, ok := sessionIDFromBase(base)
	if !ok {
		return "", fmt.Errorf("invalid filename format: %q", name)
	}
	return sessionID, nil
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

// toolCallIsError determines if a tool call status indicates an error.
// Only explicit error/failed statuses are errors; transient or empty statuses are not.
func toolCallIsError(status string) bool {
	return status == "error" || status == "failed"
}

// truncate shortens a string to at most max characters, appending "..." if truncated.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func dominantModel(counts map[string]int) string {
	var best string
	var bestN int
	for model, n := range counts {
		if n > bestN || (n == bestN && (best == "" || model < best)) {
			best = model
			bestN = n
		}
	}
	return best
}

func perMillion(cost float64, tokens int) float64 {
	if tokens == 0 {
		return 0
	}
	return cost / float64(tokens) * tokensPerMillion
}

func formatTokens(n int) string {
	if n >= tokensPerMillion {
		return fmt.Sprintf("%.1fM", float64(n)/tokensPerMillion)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
