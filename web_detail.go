package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ToolCallInfo struct {
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      bool            `json:"error,omitempty"`
	Status     string          `json:"status,omitempty"`
	DurationMs int64           `json:"durationMs,omitempty"`
}

type StepInfo struct {
	Step       int            `json:"step"`
	Timestamp  string         `json:"timestamp,omitempty"`
	Model      string         `json:"model,omitempty"`
	Input      int            `json:"input"`
	Output     int            `json:"output"`
	CacheRead  int            `json:"cacheRead"`
	CacheWrite int            `json:"cacheWrite"`
	Cost       float64        `json:"cost"`
	Thinking   string         `json:"thinking,omitempty"`
	Response   string         `json:"response,omitempty"`
	UserPrompt string         `json:"userPrompt,omitempty"`
	StopReason string         `json:"stopReason,omitempty"`
	ToolCalls  []ToolCallInfo `json:"toolCalls,omitempty"`
}

type ToolDurationStat struct {
	Name  string `json:"name"`
	AvgMs int64  `json:"avgMs"`
	MaxMs int64  `json:"maxMs"`
	Count int    `json:"count"`
}

type ModelStats struct {
	Model      string  `json:"model"`
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Steps      int     `json:"steps"`
	Cost       float64 `json:"cost"`
	CacheHit   float64 `json:"cacheHit"`
}

type SessionLink struct {
	Agent          string  `json:"agent"`
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Project        string  `json:"project,omitempty"`
	Model          string  `json:"model,omitempty"`
	TotalInput     int     `json:"totalInput,omitempty"`
	TotalOutput    int     `json:"totalOutput,omitempty"`
	TotalCacheRead int     `json:"totalCacheRead,omitempty"`
	TotalCacheWrite int    `json:"totalCacheWrite,omitempty"`
	CacheHitRate   float64 `json:"cacheHitRate,omitempty"`
	Steps          int     `json:"steps,omitempty"`
	TotalCost      float64 `json:"totalCost,omitempty"`
}

type SessionDetail struct {
	Agent     string     `json:"agent"`
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Project   string     `json:"project"`
	Model     string     `json:"model"`
	Date      string     `json:"date"`
	Duration  string     `json:"duration"`
	Steps     []StepInfo `json:"steps"`
	TotalCost float64    `json:"totalCost"`
	Parent    *SessionLink `json:"parent,omitempty"`
	Children  []SessionLink `json:"children,omitempty"`

	TotalInput       int                `json:"totalInput"`
	TotalOutput      int                `json:"totalOutput"`
	TotalCacheRead   int                `json:"totalCacheRead"`
	TotalCacheWrite  int                `json:"totalCacheWrite"`
	TotalToolCalls   int                `json:"totalToolCalls"`
	ToolErrors       int                `json:"toolErrors"`
	CacheHitRate     float64            `json:"cacheHitRate"`
	StopReasons      map[string]int     `json:"stopReasons"`
	AvgThinkingLen   int                `json:"avgThinkingLen"`
	MaxThinkingLen   int                `json:"maxThinkingLen"`
	AvgResponseLen   int                `json:"avgResponseLen"`
	MaxResponseLen   int                `json:"maxResponseLen"`
	ToolDurations    []ToolDurationStat `json:"toolDurations,omitempty"`
	ModelStats       []ModelStats       `json:"modelStats,omitempty"`
}

func fillSessionStats(d *SessionDetail) {
	if len(d.Steps) == 0 {
		return
	}
	var totalInput, totalOutput, totalCR, totalCW, totalTC, toolErrs int
	var thinkTotal, thinkMax, respTotal, respMax int
	var thinkCount, respCount int
	stopReasons := make(map[string]int)
	modelsSeen := make(map[string]bool)
	var modelsOrder []string
	modelStatsMap := make(map[string]*ModelStats)

	var firstTs, lastTs time.Time
	for i, s := range d.Steps {
		totalInput += s.Input
		totalOutput += s.Output
		totalCR += s.CacheRead
		totalCW += s.CacheWrite
		totalTC += len(s.ToolCalls)
		if s.Model != "" {
			if !modelsSeen[s.Model] {
				modelsSeen[s.Model] = true
				modelsOrder = append(modelsOrder, s.Model)
			}
			ms, ok := modelStatsMap[s.Model]
			if !ok {
				ms = &ModelStats{Model: s.Model}
				modelStatsMap[s.Model] = ms
			}
			ms.Input += s.Input
			ms.Output += s.Output
			ms.CacheRead += s.CacheRead
			ms.CacheWrite += s.CacheWrite
			ms.Cost += s.Cost
			ms.Steps++
		}
		if s.StopReason != "" {
			stopReasons[s.StopReason]++
		}
		for _, tc := range s.ToolCalls {
			if tc.Error {
				toolErrs++
			}
		}
		if s.Thinking != "" {
			l := len(s.Thinking)
			thinkTotal += l
			thinkCount++
			if l > thinkMax {
				thinkMax = l
			}
		}
		if s.Response != "" {
			l := len(s.Response)
			respTotal += l
			respCount++
			if l > respMax {
				respMax = l
			}
		}
		// parse timestamp for duration
		if s.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, s.Timestamp); err == nil {
				if i == 0 {
					firstTs = t
				}
				lastTs = t
			} else if t, err := time.Parse("2006-01-02 15:04:05", s.Timestamp); err == nil {
				if i == 0 {
					firstTs = t
				}
				lastTs = t
			}
		}
	}

	d.TotalInput = totalInput
	d.TotalOutput = totalOutput
	d.TotalCacheRead = totalCR
	d.TotalCacheWrite = totalCW
	// TotalCacheWrite stays as is (not added to totalInput)
	d.TotalToolCalls = totalTC
	d.ToolErrors = toolErrs
	d.StopReasons = stopReasons
	// Set model from all unique models found in steps
	if d.Model == "" && len(modelsOrder) > 0 {
		d.Model = strings.Join(modelsOrder, ", ")
	}
	// Build model stats array in order of appearance
	if len(modelStatsMap) > 0 {
		modelStats := make([]ModelStats, 0, len(modelStatsMap))
		for _, modelName := range modelsOrder {
			ms := modelStatsMap[modelName]
			if ms.Input+ms.CacheRead > 0 {
				ms.CacheHit = float64(ms.CacheRead) / float64(ms.Input+ms.CacheRead) * 100
			}
			modelStats = append(modelStats, *ms)
		}
		d.ModelStats = modelStats
	}
	// Cache hit rate based on token ratio (not step count)
	if totalInput+totalCR > 0 {
		d.CacheHitRate = float64(totalCR) / float64(totalInput+totalCR) * 100
	}
	if thinkCount > 0 {
		d.AvgThinkingLen = thinkTotal / thinkCount
		d.MaxThinkingLen = thinkMax
	}
	if respCount > 0 {
		d.AvgResponseLen = respTotal / respCount
		d.MaxResponseLen = respMax
	}
	// aggregate tool durations
	type durAccum struct {
		total int64
		max   int64
		count int
	}
	durByTool := make(map[string]*durAccum)
	for _, s := range d.Steps {
		for _, tc := range s.ToolCalls {
			if tc.DurationMs > 0 {
				a, ok := durByTool[tc.Name]
				if !ok {
					a = &durAccum{}
					durByTool[tc.Name] = a
				}
				a.total += tc.DurationMs
				a.count++
				if tc.DurationMs > a.max {
					a.max = tc.DurationMs
				}
			}
		}
	}
	if len(durByTool) > 0 {
		toolDurs := make([]ToolDurationStat, 0, len(durByTool))
		for name, a := range durByTool {
			toolDurs = append(toolDurs, ToolDurationStat{
				Name:  name,
				AvgMs: a.total / int64(a.count),
				MaxMs: a.max,
				Count: a.count,
			})
		}
		d.ToolDurations = toolDurs
	}

	if !firstTs.IsZero() && !lastTs.IsZero() {
		dur := lastTs.Sub(firstTs)
		if dur < time.Minute {
			d.Duration = fmt.Sprintf("%ds", int(dur.Seconds()))
		} else if dur < time.Hour {
			d.Duration = fmt.Sprintf("%dm %ds", int(dur.Minutes()), int(dur.Seconds())%60)
		} else {
			d.Duration = fmt.Sprintf("%dh %dm", int(dur.Hours()), int(dur.Minutes())%60)
		}
	}
}

func handleAPISessionDetail(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			http.Error(w, fmt.Sprintf("panic: %v", rec), http.StatusInternalServerError)
		}
	}()
	path := strings.TrimPrefix(r.URL.Path, "/api/session/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	agent, id := parts[0], parts[1]
	var detail *SessionDetail
	var err error

	switch agent {
	case "OpenCode":
		detail, err = ocSessionDetail(id)
	case "PI":
		fp, _, err2 := resolvePISessionPath(id)
		if err2 != nil {
			http.Error(w, err2.Error(), http.StatusNotFound)
			return
		}
		detail, err = piSessionDetail(fp)
	case "Claude":
		fp, _, err2 := resolveClaudeSessionPath(id)
		if err2 != nil {
			http.Error(w, err2.Error(), http.StatusNotFound)
			return
		}
		detail, err = claudeSessionDetail(fp)
	default:
		http.Error(w, "unknown agent", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	json.NewEncoder(w).Encode(detail)
}

func ocSessionDetail(sessionID string) (*SessionDetail, error) {
	db, err := openOCDB()
	if err != nil {
		return nil, err
	}

	var title, modelRaw, parentID string
	var createdAt int64
	_ = db.QueryRow("SELECT title, model, time_created, ifnull(parent_id, '') FROM session WHERE id = ?", sessionID).Scan(&title, &modelRaw, &createdAt, &parentID)
	modelName := ""
	if modelRaw != "" {
		var m struct{ ID string `json:"id"` }
		if err := json.Unmarshal([]byte(modelRaw), &m); err == nil {
			modelName = m.ID
		}
	}

	// preload message roles for this session
	msgRows, err := db.Query(`SELECT id, json_extract(data, '$.role') as role FROM message WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	msgRole := make(map[string]string) // message_id -> role
	for msgRows.Next() {
		var mid, role string
		if err := msgRows.Scan(&mid, &role); err == nil {
			msgRole[mid] = role
		}
	}
	msgRows.Close()

	rows, err := db.Query(`SELECT message_id, json(data), time_created FROM part WHERE session_id = ? ORDER BY time_created ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []StepInfo
	var current *StepInfo
	var totalCost float64
	var pendingText, pendingThinking string
	var lastUserPrompt string

	type ocPart struct {
		Type   string  `json:"type"`
		Text   string  `json:"text"`
		Reason string  `json:"reason"`
		Tool   string  `json:"tool"`
		CallID string  `json:"callID"`
		State  struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
		} `json:"state"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
		Cost  float64 `json:"cost"`
		Time  struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"time"`
	}

	for rows.Next() {
		var msgID, raw string
		var ts int64
		if err := rows.Scan(&msgID, &raw, &ts); err != nil {
			continue
		}
		var part ocPart
		if err := json.Unmarshal([]byte(raw), &part); err != nil {
			continue
		}
		role := msgRole[msgID]

		switch part.Type {
		case "text":
			t := strings.TrimSpace(part.Text)
			if t == "" {
				break
			}
			if role == "user" {
				lastUserPrompt = t
				if title == "" {
					if len(t) > 80 { title = t[:77] + "..." } else { title = t }
				}
			} else {
				if pendingText != "" {
					pendingText += "\n\n" + t
				} else {
					pendingText = t
				}
			}
		case "reasoning":
			t := strings.TrimSpace(part.Text)
			if t == "" {
				break
			}
			if pendingThinking != "" {
				pendingThinking += "\n\n" + t
			} else {
				pendingThinking = t
			}
		case "step-finish":
			stopReason := part.Reason
			if stopReason == "tool-calls" {
				stopReason = "toolUse"
			}
			s := StepInfo{
				Step:        len(steps) + 1,
				Timestamp:   time.Unix(ts/1000, (ts%1000)*1e6).UTC().Format(time.RFC3339),
				Model:       modelName,
				Input:       part.Tokens.Input,
				Output:      part.Tokens.Output,
				CacheRead:   part.Tokens.Cache.Read,
				CacheWrite:  part.Tokens.Cache.Write,
				Cost:        part.Cost,
				Thinking:    pendingThinking,
				Response:    pendingText,
				StopReason:  stopReason,
				UserPrompt:  lastUserPrompt,
			}
			lastUserPrompt = ""
			pendingText = ""
			pendingThinking = ""
			steps = append(steps, s)
			current = &steps[len(steps)-1]
			totalCost += part.Cost
		case "tool":
			if current != nil {
				var out json.RawMessage
				if b, err := json.Marshal(part.State.Output); err == nil {
					out = json.RawMessage(b)
				}
				status := part.State.Status
				isErr := status != "completed" && status != ""
				var input json.RawMessage
				if len(part.State.Input) > 0 {
					input = part.State.Input
				}
				tc := ToolCallInfo{
					Name:   part.Tool,
					Input:  input,
					Output: out,
					Error:  isErr,
					Status: status,
				}
				// tool duration from time.start/time.end if available
				if part.Time.Start > 0 && part.Time.End > 0 {
					tc.DurationMs = (part.Time.End - part.Time.Start) / 1e6
				}
				current.ToolCalls = append(current.ToolCalls, tc)
			}
		}
	}

	d := &SessionDetail{
		Agent:     "OpenCode",
		ID:        sessionID,
		Title:     title,
		Model:     modelName,
		Date:      time.Unix(createdAt/1000, 0).UTC().Format("2006-01-02 15:04"),
		Steps:     steps,
		TotalCost: totalCost,
	}
	if parentID != "" {
		var parentTitle string
		_ = db.QueryRow("SELECT title FROM session WHERE id = ?", parentID).Scan(&parentTitle)
		d.Parent = &SessionLink{Agent: "OpenCode", ID: parentID, Title: parentTitle}
	}
	childRows, err := db.Query("SELECT id, title, json_extract(model, '$.id'), ifnull(tokens_input,0), ifnull(tokens_output,0), ifnull(tokens_cache_read,0), ifnull(tokens_cache_write,0), cost FROM session WHERE parent_id = ? ORDER BY time_created ASC", sessionID)
	if err == nil {
		defer childRows.Close()
		for childRows.Next() {
			var child SessionLink
			if err := childRows.Scan(&child.ID, &child.Title, &child.Model, &child.TotalInput, &child.TotalOutput, &child.TotalCacheRead, &child.TotalCacheWrite, &child.TotalCost); err == nil {
				child.Agent = "OpenCode"
				child.Steps, _ = ocStepCount(db, child.ID)
				if child.TotalInput+child.TotalCacheRead > 0 {
					child.CacheHitRate = float64(child.TotalCacheRead) / float64(child.TotalInput+child.TotalCacheRead) * 100
				}
				d.Children = append(d.Children, child)
			}
		}
	}
	fillSessionStats(d)
	return d, rows.Err()
}

func ocStepCount(db *sql.DB, sessionID string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM part WHERE session_id = ? AND json_extract(data, '$.type') = 'step-finish'`, sessionID).Scan(&count)
	return count, err
}

func piSessionDetail(fp string) (*SessionDetail, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type piDetEntry struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Role       string `json:"role"`
			Provider   string `json:"provider"`
			Model      string `json:"model"`
			Usage      piUsage `json:"usage"`
			Content    []struct {
				Type               string          `json:"type"`
				Text               string          `json:"text"`
				Thinking           string          `json:"thinking"`
				ThinkingSignature  string          `json:"thinkingSignature"`
				Name               string          `json:"name"`
				Arguments          json.RawMessage `json:"arguments"`
				ID                 string          `json:"id"`
			} `json:"content"`
			StopReason   string `json:"stopReason"`
			ToolCallID   string `json:"toolCallId"`
			ToolName     string `json:"toolName"`
			IsError      bool   `json:"isError"`
		} `json:"message"`
	}

	var steps []StepInfo
	var title string
	var lastUserPrompt string
	var toolCallBuffer []ToolCallInfo
	toolCallTimes := make(map[string]time.Time) // toolCallId -> assistant msg timestamp
	parseTS := func(s string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02 15:04:05", s)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

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
							if len(t) > 80 {
								title = t[:77] + "..."
							} else {
								title = t
							}
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
			toolCallBuffer = append(toolCallBuffer, tc)
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
			Step:        len(steps) + 1,
			Timestamp:   entry.Timestamp,
			Model:       entry.Message.Model,
			Input:       entry.Message.Usage.Input,
			Output:      entry.Message.Usage.Output,
			CacheRead:   entry.Message.Usage.CacheRead,
			CacheWrite:  entry.Message.Usage.CacheWrite,
			Cost:        entry.Message.Usage.Cost.Total,
			StopReason:  entry.Message.StopReason,
			UserPrompt:  lastUserPrompt,
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
					Name:  c.Name,
					Input: c.Arguments,
				})
				// record tool call timestamp for duration calculation
				if c.ID != "" && !assTS.IsZero() {
					toolCallTimes[c.ID] = assTS
				}
			}
		}
		// attach buffered tool results to this step (best-effort)
		s.ToolCalls = append(s.ToolCalls, toolCallBuffer...)
		toolCallBuffer = nil
		steps = append(steps, s)
	}

	birth := getCreatedAt(fp)
	sessID := strings.TrimSuffix(filepath.Base(fp), ".jsonl")
	sessID = strings.SplitN(sessID, "_", 2)[1]
	dir := filepath.Base(filepath.Dir(fp))
	project := cleanProjectName(dir)

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
	fillSessionStats(d)
	return d, scanner.Err()
}

func claudeSessionDetail(fp string) (*SessionDetail, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type claudeContentItem struct {
		Type string          `json:"type"`
		Text string          `json:"text"`
		Name string          `json:"name"`
		ID   string          `json:"id"`
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

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		var msg claudeDetMsg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if project == "" && msg.Cwd != "" {
			project = filepath.Base(msg.Cwd)
		}

		// user messages: prompt text or tool results
		if msg.Type == "user" && msg.Message.Model == "" {
			if len(msg.Message.Content) > 0 && msg.Message.Content[0] == '[' {
				var items []struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
				}
				if err := json.Unmarshal(msg.Message.Content, &items); err == nil {
					for _, item := range items {
						if item.Type != "tool_result" || item.ToolUseID == "" {
							continue
						}
						if startTS, ok := toolCallStart[item.ToolUseID]; ok {
							if respTS, err := parseTS(msg.Timestamp); err == nil && !startTS.IsZero() {
								durMs := respTS.Sub(startTS).Milliseconds()
								for i := len(steps) - 1; i >= 0; i-- {
									matched := false
									for j := range steps[i].ToolCalls {
										if steps[i].ToolCalls[j].DurationMs == 0 {
											steps[i].ToolCalls[j].DurationMs = durMs
											matched = true
											break
										}
									}
									if matched {
										break
									}
								}
							}
							delete(toolCallStart, item.ToolUseID)
						}
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
			_ = json.Unmarshal(msg.Message.Content, &contentItems)
		}

		idx, exists := msgIndexByID[msg.Message.ID]
		if !exists {
			prices := claudePrices[msg.Message.Model]
			cost := float64(u.InputTokens)*prices.Input/1e6 +
				float64(u.CacheCreationInputTokens)*prices.CacheCreation/1e6 +
				float64(u.CacheReadInputTokens)*prices.CacheRead/1e6 +
				float64(u.OutputTokens)*prices.Output/1e6
			steps = append(steps, StepInfo{
				Step:       len(steps) + 1,
				Timestamp:  msg.Timestamp,
				Model:      msg.Message.Model,
				Input:      u.InputTokens,
				Output:     u.OutputTokens,
				CacheRead:  u.CacheReadInputTokens,
				CacheWrite: u.CacheCreationInputTokens,
				Cost:       cost,
				StopReason: msg.Message.StopReason,
				UserPrompt: lastUserPrompt,
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
				tc := ToolCallInfo{Name: c.Name, Input: c.Input}
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
				scanner := bufio.NewScanner(sf)
				if scanner.Scan() {
					var first struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					}
					if err := json.Unmarshal(scanner.Bytes(), &first); err == nil {
						if first.Message.Content != "" {
							childTitle = first.Message.Content
							if len(childTitle) > 90 {
								childTitle = childTitle[:87] + "..."
							}
						}
					}
				}
				sf.Close()
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
			}
			d.Children = append(d.Children, child)
		}
	}
	fillSessionStats(d)
	return d, scanner.Err()
}
