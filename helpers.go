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
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
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

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func repeatByte(b byte, n int) []byte {
	bs := make([]byte, n)
	for i := range bs {
		bs[i] = b
	}
	return bs
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
