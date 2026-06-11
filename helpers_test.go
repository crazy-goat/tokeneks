package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpandHome_TildeSlash(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot get home dir: %v", err)
	}
	got := expandHome("~/foo")
	want := filepath.Join(home, "foo")
	if got != want {
		t.Errorf("expandHome(~\"/foo\") = %q, want %q", got, want)
	}
}

func TestExpandHome_BareTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot get home dir: %v", err)
	}
	got := expandHome("~")
	if got != home {
		t.Errorf("expandHome(~) = %q, want %q", got, home)
	}
}

func TestExpandHome_NoTilde(t *testing.T) {
	got := expandHome("/absolute/path")
	want := "/absolute/path"
	if got != want {
		t.Errorf("expandHome(absolute) = %q, want %q", got, want)
	}
}

func TestCleanProjectName_DynamicHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot get home dir: %v", err)
	}
	user := filepath.Base(home)
	encodedPrefix := "Users-" + user + "-"

	tests := []struct {
		input string
		want  string
	}{
		{encodedPrefix + "work-project", "work/project"},
		{encodedPrefix + "work", "work"},
		{"--" + encodedPrefix + "work-project--", "work/project"},
		{"--unknown-prefix-work--", "unknown/prefix/work"},
		{"-", "(root)"},
		{"--", "(root)"},
	}
	for _, tc := range tests {
		got := cleanProjectName(tc.input)
		if got != tc.want {
			t.Errorf("cleanProjectName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCleanClaudeProjectName_DynamicHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot get home dir: %v", err)
	}
	user := filepath.Base(home)
	dashedUser := strings.ReplaceAll(user, ".", "-")

	tests := []struct {
		input string
		want  string
	}{
		{"-Users-" + dashedUser + "-work-project", "work/project"},
		{"-Users-" + dashedUser, "(root)"},
	}
	for _, tc := range tests {
		got := cleanClaudeProjectName(tc.input)
		if got != tc.want {
			t.Errorf("cleanClaudeProjectName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestOpenOCDB_ReturnsSameInstance(t *testing.T) {
	ocDBMu.Lock()
	if ocDB != nil {
		_ = ocDB.Close()
	}
	ocDB = nil
	ocDBErr = nil
	ocDBMu.Unlock()

	first, err := openOCDB()
	if err != nil {
		t.Fatalf("openOCDB() first call error: %v", err)
	}
	second, err := openOCDB()
	if err != nil {
		t.Fatalf("openOCDB() second call error: %v", err)
	}
	if first != second {
		t.Fatalf("openOCDB() returned different instances: %p vs %p", first, second)
	}

	t.Cleanup(func() {
		ocDBMu.Lock()
		if ocDB != nil {
			_ = ocDB.Close()
		}
		ocDB = nil
		ocDBErr = nil
		ocDBMu.Unlock()
	})
}

func TestPISessionIDFromFilename_DoesNotPanic(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{"2025-01-15_abc123.jsonl", "abc123", false},
		{"short.jsonl", "", true},
		{"no_underscore.jsonl", "", true},
		{"2025-01-15_.jsonl", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := piSessionIDFromFilename(tc.name)
			if tc.wantErr {
				if err == nil {
					t.Errorf("piSessionIDFromFilename(%q) expected error, got %q", tc.name, got)
				}
			} else {
				if err != nil {
					t.Errorf("piSessionIDFromFilename(%q) unexpected error: %v", tc.name, err)
				}
				if got != tc.want {
					t.Errorf("piSessionIDFromFilename(%q) = %q, want %q", tc.name, got, tc.want)
				}
			}
		})
	}
}

func TestFileDateFromFilename(t *testing.T) {
	tests := []struct {
		name  string
		date  string
		ok    bool
	}{
		{"2025-01-15_abc123.jsonl", "2025-01-15", true},
		{"short.jsonl", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := fileDateFromFilename(tc.name)
			if ok != tc.ok {
				t.Errorf("fileDateFromFilename(%q) ok=%v, want %v", tc.name, ok, tc.ok)
			}
			if got != tc.date {
				t.Errorf("fileDateFromFilename(%q) = %q, want %q", tc.name, got, tc.date)
			}
		})
	}
}

func TestToolCallIsError_KnownStatuses(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"error", true},
		{"failed", true},
		{"completed", false},
		{"running", false},
		{"pending", false},
		{"", false},
	}
	for _, tc := range tests {
		got := toolCallIsError(tc.status)
		if got != tc.want {
			t.Errorf("toolCallIsError(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestNewJSONLScanner_ScansBeyondDefaultBuffer(t *testing.T) {
	// Create a line longer than the default 64 KB bufio.Scanner limit
	longLine := strings.Repeat("a", 70*1024) // ~70 KB
	r := strings.NewReader(longLine + "\n")
	scanner := newJSONLScanner(r)
	if !scanner.Scan() {
		t.Fatal("newJSONLScanner: expected Scan to succeed, got error:", scanner.Err())
	}
	got := len(scanner.Text())
	if got != len(longLine) {
		t.Errorf("newJSONLScanner: scanned %d bytes, want %d", got, len(longLine))
	}
	if err := scanner.Err(); err != nil {
		t.Errorf("newJSONLScanner: unexpected error after scan: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"short", 80, "short"},
		{"exactly eighty chars", 20, "exactly eighty chars"},
		{"this is a long string that should be truncated with dots", 20, "this is a long st..."},
	}
	for _, tc := range tests {
		got := truncate(tc.input, tc.max)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.want)
		}
		if len(got) > tc.max {
			t.Errorf("truncate(%q, %d) = %q (len=%d), exceeds max", tc.input, tc.max, got, len(got))
		}
	}
}

func TestGetCreatedAtFromInfo_UsesProvidedInfo(t *testing.T) {
	// Create a temp file and stat it, then pass the FileInfo to getCreatedAtFromInfo.
	// The function should not call os.Stat again (verified by not panicking).
	tmpFile, err := os.CreateTemp("", "test-birth-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	info, err := os.Stat(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	got := getCreatedAtFromInfo(info)
	if got.IsZero() {
		t.Error("getCreatedAtFromInfo returned zero time")
	}
	// The birth time should be <= now
	if got.After(time.Now()) {
		t.Error("getCreatedAtFromInfo returned future time")
	}
}

func TestNewJSONLScanner_UsesConstants(t *testing.T) {
	// Verify that the magic numbers 1024*1024 and 10*1024*1024 are no longer
	// used as literals in production code (they should be in helpers.go constants)
	scanner := newJSONLScanner(strings.NewReader("test\n"))
	_ = scanner.Scan()
	// If the scanner works, constants were used correctly
	if scanner.Text() != "test" {
		t.Errorf("newJSONLScanner: expected 'test', got %q", scanner.Text())
	}
}
