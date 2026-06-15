package main

import (
	"context"
	"fmt"
	"strings"
	"time"
	"tokeneks/ingest"
	"tokeneks/store"
)

// agentToStore maps an agent display name to the lowercase name used in the store.
func agentToStore(name string) string {
	return strings.ToLower(name)
}

// parseWebTimeMs parses the time formats used by the web layer and returns
// ms since the Unix epoch. Returns 0 on failure.
func parseWebTimeMs(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// detailToStore converts a UI-shaped SessionDetail (produced by the existing
// per-agent parsers) into a store.ParsedSession ready for ingestion.
//
// The conversion emits messages in chronological order:
//   - for each step with a UserPrompt: a user message
//   - the assistant message itself
//   - for each tool call with an output: a tool message
//
// Tool call outputs are NOT duplicated on the tool_call row — they live
// exclusively on the matching role='tool' message.
func detailToStore(d *SessionDetail) (store.ParsedSession, error) {
	agent := agentToStore(d.Agent)
	createdAt := parseWebTimeMs(d.Date)
	// Last activity: max of all step timestamps, or createdAt if no steps.
	var lastActivity int64
	for _, s := range d.Steps {
		if ts := parseWebTimeMs(s.Timestamp); ts > lastActivity {
			lastActivity = ts
		}
	}
	if lastActivity == 0 {
		lastActivity = createdAt
	}
	sess := store.Session{
		Agent:        agent,
		SessionID:    d.ID,
		Project:      d.Project,
		CreatedAt:    createdAt,
		LastActivity: lastActivity,
	}

	msgs := make([]store.ParsedMessage, 0, len(d.Steps)*2)
	idx := 0
	for _, step := range d.Steps {
		ts := parseWebTimeMs(step.Timestamp)
		if step.UserPrompt != "" {
			msgs = append(msgs, store.ParsedMessage{
				Message: store.Message{
					Agent:     agent,
					SessionID: d.ID,
					MsgIndex:  idx,
					Role:      store.RoleUser,
					Content:   step.UserPrompt,
					CreatedAt: ts,
				},
			})
			idx++
		}
		content := step.Response
		if content == "" {
			content = step.Thinking
		}
		var toolCalls []store.ToolCall
		for _, tc := range step.ToolCalls {
			toolCalls = append(toolCalls, store.ToolCall{
				CallID:     tc.ID,
				Name:       tc.Name,
				Input:      string(tc.Input),
				Error:      tc.Error,
				Status:     tc.Status,
				DurationMs: tc.DurationMs,
			})
		}
		msgs = append(msgs, store.ParsedMessage{
			Message: store.Message{
				Agent:     agent,
				SessionID: d.ID,
				MsgIndex:  idx,
				Role:      store.RoleAssistant,
				Content:   content,
				Model:     step.Model,
				Thinking:  step.Thinking,
				Response:  step.Response,
				StopReason: step.StopReason,
				InputTokens:  step.Input,
				OutputTokens: step.Output,
				CacheRead:    step.CacheRead,
				CacheWrite:   step.CacheWrite,
				Cost:         step.Cost,
				CreatedAt:    ts,
			},
			ToolCalls: toolCalls,
		})
		idx++

		// tool result messages — one per tool call with output
		for _, tc := range step.ToolCalls {
			if len(tc.Output) == 0 {
				continue
			}
			toolMsgTs := ts + 1
			if tc.DurationMs > 0 {
				toolMsgTs = ts + tc.DurationMs
			}
			msgs = append(msgs, store.ParsedMessage{
				Message: store.Message{
					Agent:      agent,
					SessionID:  d.ID,
					MsgIndex:   idx,
					Role:       store.RoleTool,
					Content:    string(tc.Output),
					ToolCallID: tc.ID,
					CreatedAt:  toolMsgTs,
				},
			})
			idx++
		}
	}
	return store.ParsedSession{Session: sess, Messages: msgs}, nil
}

// claudeParser reads a Claude session JSONL file and returns a ParsedSession.
func claudeParser(ctx context.Context, ref ingest.SessionRef) (store.ParsedSession, error) {
	detail, err := claudeSessionDetail(ref.Source)
	if err != nil {
		return store.ParsedSession{}, fmt.Errorf("claude %s: %w", ref.SessionID, err)
	}
	return detailToStore(detail)
}

// piParser reads a Pi session JSONL file and returns a ParsedSession.
func piParser(ctx context.Context, ref ingest.SessionRef) (store.ParsedSession, error) {
	detail, err := piSessionDetail(ref.Source)
	if err != nil {
		return store.ParsedSession{}, fmt.Errorf("pi %s: %w", ref.SessionID, err)
	}
	return detailToStore(detail)
}

// ocParser reads an OpenCode session from the opencode DB and returns a ParsedSession.
func ocParser(ctx context.Context, ref ingest.SessionRef) (store.ParsedSession, error) {
	detail, err := ocSessionDetail(ref.SessionID)
	if err != nil {
		return store.ParsedSession{}, fmt.Errorf("opencode %s: %w", ref.SessionID, err)
	}
	return detailToStore(detail)
}
