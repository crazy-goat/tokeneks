package main

// claudeSessionDetail parses a Claude Code session JSONL file (and its
// subagent files) into a SessionDetail. Despite the shared return type this is
// not web code — see the note in oc_parse.go; the only caller is claudeParser
// in ingest_main.go.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
	"tokeneks/compute"
)

func claudeSessionDetail(fp string) (*SessionDetail, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	type claudeContentItem struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		ID    string          `json:"id"`
		Input json.RawMessage `json:"input"`
	}

	type claudeDetMsg struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID         string          `json:"id"`
			Model      string          `json:"model"`
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stop_reason"`
			Usage      struct {
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				// CacheCreation splits the total above by TTL; only the 1h
				// slice is needed since 5m is whatever remains of the total.
				CacheCreation struct {
					Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
			} `json:"usage"`
		} `json:"message,omitempty"`
		Cwd string `json:"cwd"`
	}

	var project string
	var steps []StepInfo
	var lastUserPrompt string
	toolCallStart := make(map[string]time.Time) // tool_use_id -> assistant timestamp
	msgIndexByID := make(map[string]int)
	parseTS := func(s string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02 15:04:05", s)
	}

	scanner := newJSONLScanner(f)
	skipped := 0
	defer func() { warnSkippedLines(fp, skipped) }()

	for scanner.Scan() {
		var msg claudeDetMsg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			skipped++
			continue
		}
		if project == "" && msg.Cwd != "" {
			project = filepath.Base(msg.Cwd)
		}

		// user messages: prompt text or tool results
		if msg.Type == "user" && msg.Message.Model == "" {
			if len(msg.Message.Content) > 0 && msg.Message.Content[0] == '[' {
				var items []struct {
					Type      string          `json:"type"`
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
					IsError   bool            `json:"is_error"`
				}
				if err := json.Unmarshal(msg.Message.Content, &items); err == nil {
					for _, item := range items {
						if item.Type != "tool_result" || item.ToolUseID == "" {
							continue
						}
						var startTS time.Time
						if ts, ok := toolCallStart[item.ToolUseID]; ok {
							startTS = ts
						}
						for i := len(steps) - 1; i >= 0; i-- {
							matched := false
							for j := range steps[i].ToolCalls {
								if steps[i].ToolCalls[j].ID != item.ToolUseID {
									continue
								}
								if len(item.Content) > 0 {
									steps[i].ToolCalls[j].Output = item.Content
								}
								steps[i].ToolCalls[j].Error = item.IsError
								if respTS, err := parseTS(msg.Timestamp); err == nil && !startTS.IsZero() {
									steps[i].ToolCalls[j].DurationMs = respTS.Sub(startTS).Milliseconds()
								}
								matched = true
								break
							}
							if matched {
								break
							}
						}
						delete(toolCallStart, item.ToolUseID)
					}
				}
			} else {
				var contentStr string
				if err := json.Unmarshal(msg.Message.Content, &contentStr); err == nil {
					t := strings.TrimSpace(contentStr)
					if t != "" {
						lastUserPrompt = t
					}
				}
			}
			continue
		}

		if msg.Type != "assistant" || msg.Message.Model == "" || msg.Message.ID == "" {
			continue
		}
		u := msg.Message.Usage
		if u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens+u.OutputTokens == 0 {
			continue
		}

		assTS, _ := parseTS(msg.Timestamp)
		var contentItems []claudeContentItem
		if len(msg.Message.Content) > 0 && msg.Message.Content[0] == '[' {
			_ = json.Unmarshal(msg.Message.Content, &contentItems) // best-effort; nil on failure
		}

		idx, exists := msgIndexByID[msg.Message.ID]
		if !exists {
			prices := claudeGlobalModelPrices()[msg.Message.Model]
			cost := compute.PiStepActualCost(compute.StepData{
				Input:           u.InputTokens,
				CacheCreation:   u.CacheCreationInputTokens,
				CacheCreation1h: u.CacheCreation.Ephemeral1hInputTokens,
				CacheRead:       u.CacheReadInputTokens,
				Output:          u.OutputTokens,
			}, prices)
			steps = append(steps, StepInfo{
				Step:         len(steps) + 1,
				Timestamp:    msg.Timestamp,
				Model:        msg.Message.Model,
				Input:        u.InputTokens,
				Output:       u.OutputTokens,
				CacheRead:    u.CacheReadInputTokens,
				CacheWrite:   u.CacheCreationInputTokens,
				CacheWrite1h: u.CacheCreation.Ephemeral1hInputTokens,
				Cost:         cost,
				StopReason:   msg.Message.StopReason,
				UserPrompt:   lastUserPrompt,
			})
			lastUserPrompt = ""
			idx = len(steps) - 1
			msgIndexByID[msg.Message.ID] = idx
		}

		s := &steps[idx]
		if s.Timestamp == "" {
			s.Timestamp = msg.Timestamp
		}
		if s.Model == "" {
			s.Model = msg.Message.Model
		}
		if s.StopReason == "" {
			s.StopReason = msg.Message.StopReason
		}

		for _, c := range contentItems {
			switch c.Type {
			case "thinking":
				if s.Thinking == "" {
					s.Thinking = strings.TrimSpace(c.Text)
				}
			case "text":
				text := strings.TrimSpace(c.Text)
				if text != "" {
					if s.Response == "" {
						s.Response = text
					} else if !strings.Contains(s.Response, text) {
						s.Response += "\n\n" + text
					}
				}
			case "tool_use":
				tc := ToolCallInfo{Name: c.Name, ID: c.ID, Input: c.Input}
				s.ToolCalls = append(s.ToolCalls, tc)
				if c.ID != "" && !assTS.IsZero() {
					toolCallStart[c.ID] = assTS
				}
			}
		}
	}

	birth := getCreatedAt(fp)
	sessID := strings.TrimSuffix(filepath.Base(fp), ".jsonl")
	var totalCost float64
	for _, s := range steps {
		totalCost += s.Cost
	}

	d := &SessionDetail{
		Agent:     "Claude",
		ID:        sessID,
		Project:   project,
		Date:      birth.UTC().Format("2006-01-02 15:04"),
		Steps:     steps,
		TotalCost: totalCost,
	}
	if strings.Contains(fp, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
		parentID := filepath.Base(filepath.Dir(filepath.Dir(fp)))
		if parentID != "" {
			d.Parent = &SessionLink{Agent: "Claude", ID: parentID, Project: project}
		}
	}
	baseDir := filepath.Dir(fp)
	subDir := filepath.Join(baseDir, sessID, "subagents")
	if subEntries, err := os.ReadDir(subDir); err == nil {
		for _, subEntry := range subEntries {
			if subEntry.IsDir() || filepath.Ext(subEntry.Name()) != ".jsonl" {
				continue
			}
			childID := strings.TrimSuffix(subEntry.Name(), ".jsonl")
			childTitle := childID
			if sf, err := os.Open(filepath.Join(subDir, subEntry.Name())); err == nil {
				scanner := newJSONLScanner(sf)
				if scanner.Scan() {
					var first struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					}
					if err := json.Unmarshal(scanner.Bytes(), &first); err == nil {
						if first.Message.Content != "" {
							childTitle = truncate(first.Message.Content, 90)
						}
					}
				}
				_ = sf.Close()
			}
			child := SessionLink{Agent: "Claude", ID: childID, Title: childTitle, Project: project}
			childFP := filepath.Join(subDir, subEntry.Name())
			if childDetail, err := claudeSessionDetail(childFP); err == nil {
				child.Model = childDetail.Model
				child.TotalInput = childDetail.TotalInput
				child.TotalOutput = childDetail.TotalOutput
				child.TotalCacheRead = childDetail.TotalCacheRead
				child.TotalCacheWrite = childDetail.TotalCacheWrite
				child.CacheHitRate = childDetail.CacheHitRate
				child.Steps = len(childDetail.Steps)
				child.TotalCost = childDetail.TotalCost
				child.Date = childDetail.Date
			}
			d.Children = append(d.Children, child)
		}
	}
	sortChildrenByDate(d)
	fillSessionStats(d)
	return d, scanner.Err()
}
