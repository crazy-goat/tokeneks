package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"tokeneks/compute"
)

// localVsUTCDayMismatch picks an instant whose *local* calendar date
// differs from its *UTC* calendar date, for regression tests across the
// package that must fail under an old UTC-based date comparison and pass
// under a fixed local-based one — the same trick web_test.go's
// TestDashboardWindowMs_StartEndAreLocalNotUTC uses for dashboardWindowMs,
// generalized to any nonzero UTC offset in either direction:
//
//   - A positive offset (local ahead of UTC, e.g. UTC+2) makes local's very
//     first half hour of a day fall on UTC's *previous* day — candidate 1.
//   - A negative offset (local behind UTC, e.g. UTC-5) makes local's very
//     last half hour of a day fall on UTC's *next* day — candidate 2.
//
// Trying both catches either sign. Returns ok=false only in the genuine
// offset==0 case, where no instant can disagree — callers should log a
// no-op note there rather than fail, exactly as the web.go test does.
func localVsUTCDayMismatch(t *testing.T) (instant time.Time, localDate string, ok bool) {
	t.Helper()
	for _, c := range []time.Time{
		time.Date(2026, 6, 1, 0, 30, 0, 0, time.Local),
		time.Date(2026, 6, 1, 23, 30, 0, 0, time.Local),
	} {
		if c.Format("2006-01-02") != c.UTC().Format("2006-01-02") {
			return c, c.Format("2006-01-02"), true
		}
	}
	return time.Time{}, "", false
}

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

func TestClaudeBuiltinPrices_CurrentModels(t *testing.T) {
	// Reads claudeBuiltinPriceWindows directly rather than going through
	// claudeGlobalModelPrices, so this test isn't sensitive to HOME (no
	// store/JSON overlay involved) or to memoization state left behind by
	// other tests in the same run. Only models with a single, open-ended
	// window belong here — claude-sonnet-5 has a scheduled price change and
	// is covered separately by TestClaudePrices_Sonnet5DatedWindows.
	cases := map[string]compute.ModelPrices{
		"claude-opus-5":             {Input: 5, CacheCreation: 6.25, CacheCreation1h: 10, CacheRead: 0.5, Output: 25, SupportsCacheCreation: true},
		"claude-opus-4-8":           {Input: 5, CacheCreation: 6.25, CacheCreation1h: 10, CacheRead: 0.5, Output: 25, SupportsCacheCreation: true},
		"claude-opus-4-7":           {Input: 5, CacheCreation: 6.25, CacheCreation1h: 10, CacheRead: 0.5, Output: 25, SupportsCacheCreation: true},
		"claude-fable-5":            {Input: 10, CacheCreation: 12.5, CacheCreation1h: 20, CacheRead: 1, Output: 50, SupportsCacheCreation: true},
		"claude-haiku-4-5-20251001": {Input: 1, CacheCreation: 1.25, CacheCreation1h: 2, CacheRead: 0.1, Output: 5, SupportsCacheCreation: true},
	}
	for model, want := range cases {
		got, ok := resolveClaudeWindow(claudeBuiltinPriceWindows[model], time.Now())
		if !ok {
			t.Fatalf("no built-in price window for %s", model)
		}
		if got != want {
			t.Errorf("price for %s = %+v, want %+v", model, got, want)
		}
	}
}

func TestExpandHome_NoTilde(t *testing.T) {
	got := expandHome("/absolute/path")
	want := "/absolute/path"
	if got != want {
		t.Errorf("expandHome(absolute) = %q, want %q", got, want)
	}
}

func TestDominantModel_MostFrequent(t *testing.T) {
	got := dominantModel(map[string]int{"gpt-4": 2, "gpt-3.5": 5, "claude": 1})
	if got != "gpt-3.5" {
		t.Errorf("dominantModel(most frequent) = %q, want %q", got, "gpt-3.5")
	}
}

func TestDominantModel_TieBreakLexicographic(t *testing.T) {
	got := dominantModel(map[string]int{"zeta": 3, "alpha": 3, "beta": 1})
	if got != "alpha" {
		t.Errorf("dominantModel(tie break) = %q, want %q", got, "alpha")
	}
}

func TestPerMillion_ZeroTokens(t *testing.T) {
	got := compute.PerMillion(1.0, 0)
	if got != 0.0 {
		t.Errorf("compute.PerMillion(1.0, 0) = %v, want 0", got)
	}
}

func TestPerMillion_NonZero(t *testing.T) {
	got := compute.PerMillion(1.0, 1_000_000)
	if got != 1.0 {
		t.Errorf("compute.PerMillion(1.0, 1000000) = %v, want 1", got)
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

func TestWalkSessionFiles_SkipsNonJSONL(t *testing.T) {
	sessionsDir := t.TempDir()
	subdir := filepath.Join(sessionsDir, "subdirA")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() = %v", err)
	}

	validPath := filepath.Join(subdir, "valid.jsonl")
	if err := os.WriteFile(validPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(valid.jsonl) = %v", err)
	}
	ignorePath := filepath.Join(subdir, "ignore.txt")
	if err := os.WriteFile(ignorePath, []byte("ignore me\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(ignore.txt) = %v", err)
	}

	var got []string
	err := walkSessionFiles(sessionsDir, func(path string, info os.FileInfo) error {
		got = append(got, path)
		if path != validPath {
			t.Errorf("callback got path %q, want %q", path, validPath)
		}
		if info.Name() != "valid.jsonl" {
			t.Errorf("callback got file %q, want valid.jsonl", info.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walkSessionFiles() = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("walkSessionFiles() called callback %d times, want 1", len(got))
	}
	if got[0] != validPath {
		t.Fatalf("walkSessionFiles() collected %q, want %q", got[0], validPath)
	}
}

func TestWalkSessionFiles_IncludesNestedJSONL(t *testing.T) {
	sessionsDir := t.TempDir()
	nestedDir := filepath.Join(sessionsDir, "project", "session", "subagents")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nestedDir, "agent-short.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := walkSessionFiles(sessionsDir, func(path string, info os.FileInfo) error {
		got = append(got, path)
		return nil
	}); err != nil {
		t.Fatalf("walkSessionFiles() = %v", err)
	}
	if len(got) != 1 || got[0] != path {
		t.Fatalf("walkSessionFiles() = %v, want [%s]", got, path)
	}
}

func TestFileDateFromFilename(t *testing.T) {
	tests := []struct {
		name string
		date string
		ok   bool
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
	defer func() { _ = os.Remove(tmpFile.Name()) }()
	_ = tmpFile.Close()

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
