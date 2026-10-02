package main

// ocSessionDetail and its helper parse an OpenCode session straight out of
// OpenCode's own SQLite DB into a SessionDetail. Despite the shared return
// type, this is not web code: the dashboard reads sessions from the tokeneks
// store (getSessionDetailFromStore), and the only caller here is ocParser in
// ingest_main.go, which turns the result into a store.ParsedSession. It lived
// in web_detail.go for historical reasons, back when the web layer parsed
// sources directly.

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

func ocSessionDetail(sessionID string) (*SessionDetail, error) {
	db, err := openOCDB()
	if err != nil {
		return nil, err
	}

	var title, modelRaw, parentID, projectName string
	var createdAt int64
	var modelName string
	// Non-row errors are unexpected; proceed with zero values.
	_ = db.QueryRow(`
		SELECT s.title, s.model, s.time_created, ifnull(s.parent_id, ''), coalesce(p.name, p.worktree, '')
		FROM session s
		LEFT JOIN project p ON p.id = s.project_id
		WHERE s.id = ?
	`, sessionID).Scan(&title, &modelRaw, &createdAt, &parentID, &projectName)
	if modelRaw != "" {
		var m struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(modelRaw), &m); err == nil {
			modelName = m.ID
		}
	}

	// preload message roles and per-message model for this session
	msgRows, err := db.Query(`SELECT id, json_extract(data, '$.role') as role, ifnull(json_extract(data, '$.modelID'), json_extract(data, '$.model.modelID')) as model FROM message WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	msgRole := make(map[string]string)  // message_id -> role
	msgModel := make(map[string]string) // message_id -> modelID
	for msgRows.Next() {
		var mid, role, modelID string
		if err := msgRows.Scan(&mid, &role, &modelID); err == nil {
			msgRole[mid] = role
			if modelID != "" {
				msgModel[mid] = modelID
			}
		}
	}
	msgRows.Close()

	rows, err := db.Query(`SELECT message_id, json(data), time_created FROM part WHERE session_id = ? ORDER BY time_created ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	skipped := 0
	defer func() { warnSkippedLines("OpenCode session "+sessionID, skipped) }()

	var steps []StepInfo
	curIdx := -1
	msgToStepIdx := make(map[string]int)
	var totalCost float64
	var pendingText, pendingThinking string
	var lastUserPrompt string

	type ocPart struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Reason string `json:"reason"`
		Tool   string `json:"tool"`
		CallID string `json:"callID"`
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
		Cost float64 `json:"cost"`
		Time struct {
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
			skipped++
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
					title = truncate(t, 80)
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
			stepModel := modelName
			if m, ok := msgModel[msgID]; ok && m != "" {
				stepModel = m
			}
			s := StepInfo{
				Step:       len(steps) + 1,
				Timestamp:  time.Unix(ts/1000, (ts%1000)*int64(time.Millisecond)).UTC().Format(time.RFC3339),
				Model:      stepModel,
				Input:      part.Tokens.Input,
				Output:     part.Tokens.Output,
				CacheRead:  part.Tokens.Cache.Read,
				CacheWrite: part.Tokens.Cache.Write,
				Cost:       part.Cost,
				Thinking:   pendingThinking,
				Response:   pendingText,
				StopReason: stopReason,
				UserPrompt: lastUserPrompt,
			}
			lastUserPrompt = ""
			pendingText = ""
			pendingThinking = ""
			steps = append(steps, s)
			curIdx = len(steps) - 1
			msgToStepIdx[msgID] = curIdx
			totalCost += part.Cost
		case "tool":
			idx, ok := msgToStepIdx[msgID]
			if !ok {
				idx = curIdx
			}
			if idx >= 0 {
				var out json.RawMessage
				if b, err := json.Marshal(part.State.Output); err == nil {
					out = json.RawMessage(b)
				}
				status := part.State.Status
				isErr := toolCallIsError(status)
				var input json.RawMessage
				if len(part.State.Input) > 0 {
					input = part.State.Input
				}
				tc := ToolCallInfo{
					Name:   part.Tool,
					ID:     part.CallID,
					Input:  input,
					Output: out,
					Error:  isErr,
					Status: status,
				}
				// tool duration from time.start/time.end if available
				if part.Time.Start > 0 && part.Time.End > 0 {
					tc.DurationMs = (part.Time.End - part.Time.Start) / int64(time.Millisecond)
				}
				steps[idx].ToolCalls = append(steps[idx].ToolCalls, tc)
			}
		}
	}

	d := &SessionDetail{
		Agent:     "OpenCode",
		ID:        sessionID,
		Title:     title,
		Project:   projectName,
		Model:     modelName,
		Date:      time.Unix(createdAt/1000, 0).UTC().Format("2006-01-02 15:04"),
		Steps:     steps,
		TotalCost: totalCost,
	}
	if parentID != "" {
		var parentTitle string
		// Best-effort parent title lookup; leave empty on failure.
		_ = db.QueryRow("SELECT title FROM session WHERE id = ?", parentID).Scan(&parentTitle)
		d.Parent = &SessionLink{Agent: "OpenCode", ID: parentID, Title: parentTitle}
	}
	childRows, err := db.Query("SELECT id, title, json_extract(model, '$.id'), ifnull(tokens_input,0), ifnull(tokens_output,0), ifnull(tokens_cache_read,0), ifnull(tokens_cache_write,0), cost, ifnull(time_created,0) FROM session WHERE parent_id = ? ORDER BY time_created ASC", sessionID)
	if err == nil {
		defer childRows.Close()
		for childRows.Next() {
			var child SessionLink
			var childCreated int64
			if err := childRows.Scan(&child.ID, &child.Title, &child.Model, &child.TotalInput, &child.TotalOutput, &child.TotalCacheRead, &child.TotalCacheWrite, &child.TotalCost, &childCreated); err == nil {
				child.Agent = "OpenCode"
				if childCreated > 0 {
					child.Date = time.Unix(childCreated/1000, 0).UTC().Format("2006-01-02 15:04")
				}
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
