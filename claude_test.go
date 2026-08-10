package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"tokeneks/compute"
)

// writeClaudeJSONL writes lines (already-serialized JSON, one per line) to a
// fresh file under t.TempDir() and returns its path.
func writeClaudeJSONL(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// claudeLine builds one assistant-message JSONL line. usage is repeated
// verbatim across every content-block line of a real message, per the bug
// claudeMessages works around — tests that want to exercise dedup pass the
// same id/usage to several calls.
func claudeLine(id, model string, input, output int, content string) string {
	return `{"type":"assistant","message":{"id":"` + id + `","model":"` + model +
		`","usage":{"input_tokens":` + strconv.Itoa(input) + `,"output_tokens":` + strconv.Itoa(output) +
		`},"content":` + content + `},"timestamp":"2026-08-03T16:00:00.000Z"}`
}

func TestClaudeMessages_MultiBlockSameID_CountsUsageOnce(t *testing.T) {
	// Claude Code writes one line per content block; a thinking block, a
	// text block, and a tool call from the same assistant turn all repeat
	// the identical message.usage. That must collapse into one step.
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"thinking"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"tool_use"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(res.Steps))
	}
	want := compute.StepData{Input: 100, Output: 50}
	if res.Steps[0].Step != want {
		t.Errorf("Steps[0].Step = %+v, want %+v (usage must be counted once, not per block)", res.Steps[0].Step, want)
	}
	if len(res.Models) != 1 {
		t.Errorf("Models = %v, want exactly one entry (one per message, not per block)", res.Models)
	}
}

func TestClaudeMessages_ToolCallsSurviveDedup(t *testing.T) {
	// Three tool_use blocks spread across three lines of one message must
	// still add up to three tool calls, even though the lines collapse into
	// a single step.
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"tool_use"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"tool_use"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"tool_use"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(res.Steps))
	}
	if res.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3", res.ToolCalls)
	}
}

func TestClaudeMessages_DistinctIDsStayDistinctSteps(t *testing.T) {
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
		claudeLine("msg2", "claude-sonnet-5", 200, 75, `[{"type":"text"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2", len(res.Steps))
	}
	if len(res.Models) != 2 {
		t.Errorf("Models = %v, want 2 entries", res.Models)
	}
}

func TestClaudeMessages_EmptyIDsNotMergedTogether(t *testing.T) {
	// An empty message.id can't be used as a merge key, so unlike a shared
	// real id, two id-less lines must never collapse into one step — that
	// would conflate two unrelated messages. Each stays its own step.
	path := writeClaudeJSONL(t, []string{
		claudeLine("", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
		claudeLine("", "claude-sonnet-5", 200, 75, `[{"type":"text"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2 (id-less lines must not merge with each other)", len(res.Steps))
	}
}

func TestClaudeMessages_ZeroUsageLineDoesNotCreatePhantomOrZeroRealUsage(t *testing.T) {
	// A defensive case: a zero-usage line sharing an id with a real line
	// must not spawn an extra ("phantom") step, and must not zero out the
	// real usage either. This isn't known to happen in real session files
	// (usage is identical across every block of one message there), but the
	// dedup logic must not depend on that to stay correct.
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 0, 0, `[{"type":"thinking"}]`),
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1 (zero-usage line must not create a phantom step)", len(res.Steps))
	}
	want := compute.StepData{Input: 100, Output: 50}
	if res.Steps[0].Step != want {
		t.Errorf("Steps[0].Step = %+v, want %+v (zero-usage line must not zero out the real one)", res.Steps[0].Step, want)
	}
}

func TestClaudeMessages_ZeroUsageLineFirst_RealLineStillCreatesStep(t *testing.T) {
	// Same as above but with the zero-usage line arriving first in the
	// file, to make sure the "first line for an id" isn't assumed to be the
	// one that determines the step's usage.
	path := writeClaudeJSONL(t, []string{
		claudeLine("msg1", "claude-sonnet-5", 100, 50, `[{"type":"text"}]`),
		claudeLine("msg1", "claude-sonnet-5", 0, 0, `[{"type":"thinking"}]`),
	})

	res, err := claudeMessages(path)
	if err != nil {
		t.Fatalf("claudeMessages() = %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(res.Steps))
	}
	want := compute.StepData{Input: 100, Output: 50}
	if res.Steps[0].Step != want {
		t.Errorf("Steps[0].Step = %+v, want %+v", res.Steps[0].Step, want)
	}
}
