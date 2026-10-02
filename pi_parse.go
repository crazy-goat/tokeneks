package main

// piSessionDetail parses a PI Agent session JSONL file (and its subsession
// files) into a SessionDetail. Despite the shared return type this is not web
// code — see the note in oc_parse.go; the only caller is piParser in
// ingest_main.go.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func piSessionDetail(fp string) (*SessionDetail, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	type piDetEntry struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Role     string  `json:"role"`
			Provider string  `json:"provider"`
			Model    string  `json:"model"`
			Usage    piUsage `json:"usage"`
			Content  []struct {
				Type              string          `json:"type"`
				Text              string          `json:"text"`
				Thinking          string          `json:"thinking"`
				ThinkingSignature string          `json:"thinkingSignature"`
				Name              string          `json:"name"`
				Arguments         json.RawMessage `json:"arguments"`
				ID                string          `json:"id"`
			} `json:"content"`
			StopReason string `json:"stopReason"`
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
			IsError    bool   `json:"isError"`
		} `json:"message"`
	}

	var steps []StepInfo
	var title string
	var lastUserPrompt string
	toolCallTimes := make(map[string]time.Time) // toolCallId -> assistant msg timestamp
	parseTS := func(s string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02 15:04:05", s)
	}

	scanner := newJSONLScanner(f)

	for scanner.Scan() {
		var entry piDetEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != "message" {
			continue
		}
		if entry.Message.Role == "user" {
			for _, c := range entry.Message.Content {
				if c.Type == "text" {
					t := strings.TrimSpace(c.Text)
					if t != "" {
						// store full prompt for the next assistant step
						lastUserPrompt = t
						// first user message also sets the session title
						if title == "" {
							title = truncate(t, 80)
						}
						break
					}
				}
			}
			continue
		}
		if entry.Message.Role == "toolResult" {
			var out json.RawMessage
			if len(entry.Message.Content) > 0 {
				if entry.Message.Content[0].Type == "text" {
					if b, err := json.Marshal(entry.Message.Content[0].Text); err == nil {
						out = json.RawMessage(b)
					}
				} else {
					b, _ := json.Marshal(entry.Message.Content)
					out = b
				}
			}
			tc := ToolCallInfo{
				ID:     entry.Message.ToolCallID,
				Name:   entry.Message.ToolName,
				Output: out,
				Error:  entry.Message.IsError,
			}
			// compute tool duration from recorded timestamps
			if reqTS, ok := toolCallTimes[entry.Message.ToolCallID]; ok {
				if respTS, err := parseTS(entry.Timestamp); err == nil {
					tc.DurationMs = respTS.Sub(reqTS).Milliseconds()
				}
			}
			delete(toolCallTimes, entry.Message.ToolCallID)
			for i := len(steps) - 1; i >= 0; i-- {
				matched := false
				for j := range steps[i].ToolCalls {
					if steps[i].ToolCalls[j].ID != tc.ID {
						continue
					}
					steps[i].ToolCalls[j].Output = tc.Output
					steps[i].ToolCalls[j].Error = tc.Error
					steps[i].ToolCalls[j].DurationMs = tc.DurationMs
					matched = true
					break
				}
				if matched {
					break
				}
			}
			continue
		}
		if entry.Message.Model == "" {
			continue
		}
		if entry.Message.Usage.Total == 0 {
			continue
		}

		assTS, _ := parseTS(entry.Timestamp)

		s := StepInfo{
			Step:       len(steps) + 1,
			Timestamp:  entry.Timestamp,
			Model:      entry.Message.Model,
			Input:      entry.Message.Usage.Input,
			Output:     entry.Message.Usage.Output,
			CacheRead:  entry.Message.Usage.CacheRead,
			CacheWrite: entry.Message.Usage.CacheWrite,
			Cost:       entry.Message.Usage.Cost.Total,
			StopReason: entry.Message.StopReason,
			UserPrompt: lastUserPrompt,
		}
		lastUserPrompt = "" // consumed
		// extract thinking, text and tool calls from assistant message content
		for _, c := range entry.Message.Content {
			switch c.Type {
			case "thinking":
				s.Thinking = c.Thinking
			case "text":
				s.Response = c.Text
			case "toolCall":
				s.ToolCalls = append(s.ToolCalls, ToolCallInfo{
					ID:    c.ID,
					Name:  c.Name,
					Input: c.Arguments,
				})
				// record tool call timestamp for duration calculation
				if c.ID != "" && !assTS.IsZero() {
					toolCallTimes[c.ID] = assTS
				}
			}
		}
		steps = append(steps, s)
	}

	birth := getCreatedAt(fp)
	sessID := piSessionIDFromPath(fp)
	projectDir := filepath.Base(filepath.Dir(fp))
	project := cleanProjectName(projectDir)
	// Subagent sessions inherit the parent's project — for the nested layout
	// the session file itself lives below the project directory, so the name
	// has to be taken from the parent's path.
	var parent *SessionLink
	if parentPath, parentID, ok := piResolveParent(fp); ok {
		projectDir = filepath.Base(filepath.Dir(parentPath))
		project = cleanProjectName(projectDir)
		parentTitle := project
		if parentData, err := piSessionUsage(parentPath); err == nil && parentData.Title != "" {
			parentTitle = parentData.Title
		}
		parent = &SessionLink{Agent: "PI", ID: parentID, Title: parentTitle, Project: project}
	}

	var totalCost float64
	for _, s := range steps {
		totalCost += s.Cost
	}

	d := &SessionDetail{
		Agent:     "PI",
		ID:        sessID,
		Title:     title,
		Project:   project,
		Date:      birth.UTC().Format("2006-01-02 15:04"),
		Steps:     steps,
		TotalCost: totalCost,
	}
	d.Parent = parent
	for _, childPath := range piSubsessionPaths(fp) {
		childDetail, err := piSessionDetail(childPath)
		if err != nil {
			continue
		}
		childTitle := childDetail.Title
		if childTitle == "" {
			childTitle = childDetail.Project
		}
		d.Children = append(d.Children, SessionLink{
			Agent:           "PI",
			ID:              childDetail.ID,
			Title:           childTitle,
			Project:         childDetail.Project,
			Model:           childDetail.Model,
			Date:            childDetail.Date,
			TotalInput:      childDetail.TotalInput,
			TotalOutput:     childDetail.TotalOutput,
			TotalCacheRead:  childDetail.TotalCacheRead,
			TotalCacheWrite: childDetail.TotalCacheWrite,
			CacheHitRate:    childDetail.CacheHitRate,
			Steps:           len(childDetail.Steps),
			TotalCost:       childDetail.TotalCost,
		})
	}
	sortChildrenByDate(d)
	fillSessionStats(d)
	return d, scanner.Err()
}
