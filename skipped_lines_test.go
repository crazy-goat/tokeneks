package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureSkippedWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := skippedLinesWarnOut
	skippedLinesWarnOut = &buf
	skippedLinesWarnMu.Lock()
	skippedLinesWarned = map[string]int{}
	skippedLinesWarnMu.Unlock()
	t.Cleanup(func() { skippedLinesWarnOut = prev })
	return &buf
}

func TestWarnSkippedLines_ZeroIsSilent(t *testing.T) {
	buf := captureSkippedWarnings(t)
	warnSkippedLines("x.jsonl", 0)
	if buf.Len() != 0 {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestWarnSkippedLines_OncePerCount(t *testing.T) {
	buf := captureSkippedWarnings(t)
	warnSkippedLines("x.jsonl", 2)
	warnSkippedLines("x.jsonl", 2)
	if got := strings.Count(buf.String(), "skipped 2 unparseable line(s) in x.jsonl"); got != 1 {
		t.Fatalf("want one warning, got %d: %q", got, buf.String())
	}
	warnSkippedLines("x.jsonl", 3)
	if !strings.Contains(buf.String(), "skipped 3 unparseable line(s)") {
		t.Fatalf("changed count must warn again: %q", buf.String())
	}
}

func TestClaudeMessages_WarnOnMalformedUserContent(t *testing.T) {
	for _, content := range []string{`{}`, `42`, `[{"type":"text","text":42}]`} {
		t.Run(content, func(t *testing.T) {
			buf := captureSkippedWarnings(t)
			path := writeClaudeJSONL(t, []string{
				`{"type":"user","message":{"content":"valid prompt"}}`,
				`{"type":"user","message":{"content":` + content + `}}`,
				`{"type":"user","message":{"content":null}}`,
				`{"type":"user","message":{}}`,
				`{"type":"user","message":{"content":[{"type":"tool_result","content":"done"}]}}`,
			})
			res, err := claudeMessages(path)
			if err != nil {
				t.Fatal(err)
			}
			if res.LastUserPrompt != "valid prompt" {
				t.Errorf("LastUserPrompt = %q, want valid prompt", res.LastUserPrompt)
			}
			if !strings.Contains(buf.String(), "skipped 1 unparseable line(s) in "+path) {
				t.Fatalf("missing warning: %q", buf.String())
			}
		})
	}
}

func TestSessionParsers_WarnOnCorruptLines(t *testing.T) {
	good := `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`
	cases := map[string]func(string){
		"claudeMessages":      func(p string) { _, _ = claudeMessages(p) },
		"claudeSessionDetail": func(p string) { _, _ = claudeSessionDetail(p) },
		"piSessionUsage":      func(p string) { _, _ = piSessionUsage(p) },
		"piSessionDetail":     func(p string) { _, _ = piSessionDetail(p) },
	}
	for name, parse := range cases {
		t.Run(name, func(t *testing.T) {
			buf := captureSkippedWarnings(t)
			path := filepath.Join(t.TempDir(), "s.jsonl")
			content := good + "\n{not json\n" + good + "\n{\"truncated\":\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			parse(path)
			if !strings.Contains(buf.String(), "skipped 2 unparseable line(s) in "+path) {
				t.Fatalf("missing warning: %q", buf.String())
			}
		})
	}
}
