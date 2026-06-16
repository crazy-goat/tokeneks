package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"tokeneks/compute"
	"tokeneks/ingest"
	"tokeneks/store"
)

// tokeneksStore is the process-wide store used by web and CLI handlers.
// Opened by openTokeneksStore, closed at process exit.
var (
	tokeneksStoreMu sync.Mutex
	tokeneksStore   *store.Store
)

// setTokeneksStore installs the global store (used by web and CLI).
func setTokeneksStore(s *store.Store) {
	tokeneksStoreMu.Lock()
	defer tokeneksStoreMu.Unlock()
	tokeneksStore = s
}

func getTokeneksStore() *store.Store {
	tokeneksStoreMu.Lock()
	defer tokeneksStoreMu.Unlock()
	return tokeneksStore
}

// agentDisplayName maps the lowercase store agent name to the display name
// expected by WebSession / SessionDetail (e.g. "opencode" -> "OpenCode").
func agentDisplayName(agent string) string {
	switch agent {
	case "opencode":
		return "OpenCode"
	case "pi":
		return "PI"
	case "claude":
		return "Claude"
	}
	return agent
}

// gatherWebSessionsFromStore returns the list of sessions to display in the
// web dashboard, computed entirely from the local store.
// Sessions with no messages are excluded: the watcher keeps them in the
// store purely as a mtime-filter baseline (so the watcher doesn't keep
// re-parsing them on every poll), but they're never meant to surface in
// any list — see ingest/watcher.go reingestRef.
func gatherWebSessionsFromStore(ctx context.Context, days int) ([]WebSession, error) {
	st := getTokeneksStore()
	if st == nil {
		return nil, fmt.Errorf("store not open")
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	rows, err := st.DB().QueryContext(ctx, `
		SELECT
		  s.agent, s.session_id, s.project, COALESCE(s.parent_id, ''),
		  s.created_at, s.last_activity,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.input_tokens  END), 0) AS in_tok,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.output_tokens END), 0) AS out_tok,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.cache_read   END), 0) AS cr,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.cache_write  END), 0) AS cw,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.cost         END), 0) AS cost,
		  COUNT(CASE WHEN m.role='assistant' THEN 1 END) AS msg_count
		FROM session s
		LEFT JOIN message m ON m.agent = s.agent AND m.session_id = s.session_id
		WHERE s.last_activity >= ?
		  AND EXISTS (SELECT 1 FROM message m2
		              WHERE m2.agent = s.agent AND m2.session_id = s.session_id)
		GROUP BY s.agent, s.session_id
		ORDER BY s.last_activity DESC
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessionsByKey := make(map[sessKey]*WebSession)
	keys := []sessKey{}

	for rows.Next() {
		var (
			agent, id, project, parentID string
			created, last                int64
			inT, outT, cr, cw            int
			cost                         float64
			msgCount                     int
		)
		if err := rows.Scan(&agent, &id, &project, &parentID, &created, &last,
			&inT, &outT, &cr, &cw, &cost, &msgCount); err != nil {
			return nil, err
		}
		ws := &WebSession{
			Agent:           agentDisplayName(agent),
			ID:              id,
			Date:            time.UnixMilli(created).UTC().Format("2006-01-02 15:04"),
			Project:         project,
			DominantModel:   "",
			LastMessage:     time.UnixMilli(last).UTC().Format("2006-01-02 15:04:05"),
			TotalInput:      inT,
			TotalOutput:     outT,
			TotalCacheRead:  cr,
			TotalCacheWrite: cw,
			TotalCost:       cost,
			Messages:        msgCount,
			PromptInput:     inT + cw,
			ParentID:        parentID,
			IsSubsession:    parentID != "",
			Models:          []WebModelUsage{},
		}
		sessionsByKey[sessKey{agent, id}] = ws
		keys = append(keys, sessKey{agent, id})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return []WebSession{}, nil
	}

	// Per-model, tool-count and child-count are computed for ALL sessions in
	// the DB (filtered by date) and joined in Go. Passing a per-session
	// `(agent=?, session_id=?) OR ...` list with hundreds of keys was the
	// reason "last 30 days" used to run for many seconds.
	perModel, err := perModelUsage(ctx, st, cutoff)
	if err != nil {
		return nil, err
	}
	toolCounts, err := toolCallCounts(ctx, st, cutoff)
	if err != nil {
		return nil, err
	}
	childCounts, err := childSessionCounts(ctx, st)
	if err != nil {
		return nil, err
	}

	out := make([]WebSession, 0, len(keys))
	for _, k := range keys {
		ws := sessionsByKey[k]
		models := perModel[k]
		if models == nil {
			models = []WebModelUsage{}
		}
		ws.Models = models
		ws.ToolCalls = toolCounts[k]
		ws.ChildCount = childCounts[k]
		if len(ws.Models) > 0 {
			ws.DominantModel = ws.Models[0].Model
		}
		out = append(out, *ws)
	}
	return out, nil
}

type sessKey struct{ agent, id string }

func perModelUsage(ctx context.Context, st *store.Store, cutoffMs int64) (map[sessKey][]WebModelUsage, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT m.agent, m.session_id, m.model, COALESCE(m.provider, ''),
		       COALESCE(SUM(m.input_tokens),  0),
		       COALESCE(SUM(m.output_tokens), 0),
		       COALESCE(SUM(m.cache_read),    0),
		       COALESCE(SUM(m.cache_write),   0),
		       COALESCE(SUM(m.cost),          0),
		       COUNT(m.id)
		FROM message m
		JOIN session s ON s.agent = m.agent AND s.session_id = m.session_id
		WHERE m.role = 'assistant' AND s.last_activity >= ?
		GROUP BY m.agent, m.session_id, m.model
	`, cutoffMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[sessKey][]WebModelUsage)
	for rows.Next() {
		var (
			agent, id, model, provider string
			inT, outT, cr, cw          int
			cost                       float64
			count                      int
		)
		if err := rows.Scan(&agent, &id, &model, &provider, &inT, &outT, &cr, &cw, &cost, &count); err != nil {
			return nil, err
		}
		k := sessKey{agent, id}
		out[k] = append(out[k], WebModelUsage{
			Model: model, Provider: provider,
			Input: inT, Output: outT, CacheRead: cr, CacheWrite: cw,
			Cost: cost, Messages: count,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k := range out {
		models := out[k]
		sort.Slice(models, func(i, j int) bool { return models[i].Cost > models[j].Cost })
		out[k] = models
	}
	return out, nil
}

func toolCallCounts(ctx context.Context, st *store.Store, cutoffMs int64) (map[sessKey]int, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT m.agent, m.session_id, COUNT(tc.id)
		FROM message m
		JOIN session s ON s.agent = m.agent AND s.session_id = m.session_id
		LEFT JOIN tool_call tc ON tc.message_id = m.id
		WHERE m.role = 'assistant' AND s.last_activity >= ?
		GROUP BY m.agent, m.session_id
	`, cutoffMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[sessKey]int)
	for rows.Next() {
		var agent, id string
		var n int
		if err := rows.Scan(&agent, &id, &n); err != nil {
			return nil, err
		}
		out[sessKey{agent, id}] = n
	}
	return out, rows.Err()
}

func childSessionCounts(ctx context.Context, st *store.Store) (map[sessKey]int, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT agent, parent_id, COUNT(*)
		FROM session
		WHERE parent_id IS NOT NULL AND parent_id != ''
		GROUP BY agent, parent_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[sessKey]int)
	for rows.Next() {
		var agent, parent string
		var n int
		if err := rows.Scan(&agent, &parent, &n); err != nil {
			return nil, err
		}
		out[sessKey{agent, parent}] = n
	}
	return out, rows.Err()
}

// getSessionDetailFromStore returns the SessionDetail for one session,
// reading entirely from the local store.
func getSessionDetailFromStore(ctx context.Context, agent, sessionID string) (*SessionDetail, error) {
	st := getTokeneksStore()
	if st == nil {
		return nil, fmt.Errorf("store not open")
	}
	agent = strings.ToLower(agent)
	sess, err := st.GetSession(ctx, agent, sessionID)
	if err != nil {
		return nil, err
	}
	msgs, err := st.GetMessages(ctx, agent, sessionID)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no messages for session %s/%s", agent, sessionID)
	}

	detail := &SessionDetail{
		Agent:   agentDisplayName(agent),
		ID:      sessionID,
		Project: sess.Project,
		Date:    time.UnixMilli(sess.CreatedAt).UTC().Format("2006-01-02 15:04"),
	}

	// Convert messages to steps. Each assistant message is one step.
	// User/tool messages are merged into the adjacent step's UserPrompt / ToolCalls.
	steps := []StepInfo{}
	for _, m := range msgs {
		switch m.Role {
		case store.RoleUser:
			if len(steps) == 0 {
				continue
			}
			steps[len(steps)-1].UserPrompt = m.Content
		case store.RoleAssistant:
			s := StepInfo{
				Step:       len(steps) + 1,
				Timestamp:  time.UnixMilli(m.CreatedAt).UTC().Format(time.RFC3339),
				Model:      m.Model,
				Input:      m.InputTokens,
				Output:     m.OutputTokens,
				CacheRead:  m.CacheRead,
				CacheWrite: m.CacheWrite,
				Cost:       m.Cost,
				Thinking:   m.Thinking,
				Response:   m.Response,
				StopReason: m.StopReason,
			}
			tcs, err := st.GetToolCalls(ctx, m.ID)
			if err != nil {
				return nil, err
			}
			for _, tc := range tcs {
				s.ToolCalls = append(s.ToolCalls, ToolCallInfo{
					Name:       tc.Name,
					ID:         tc.CallID,
					Input:      json.RawMessage(tc.Input),
					Error:      tc.Error,
					Status:     tc.Status,
					DurationMs: tc.DurationMs,
				})
				// attach tool result from the matching role=tool message
				for _, m2 := range msgs {
					if m2.Role == store.RoleTool && m2.ToolCallID == tc.CallID {
						s.ToolCalls[len(s.ToolCalls)-1].Output = json.RawMessage(m2.Content)
						break
					}
				}
			}
			steps = append(steps, s)
		}
	}
	detail.Steps = steps

	// children
	childRows, err := st.DB().QueryContext(ctx, `
		SELECT s.session_id, COALESCE(s.project, ''),
		       COALESCE((SELECT m.model FROM message m
		                 WHERE m.agent = s.agent AND m.session_id = s.session_id
		                 AND m.role = 'assistant' ORDER BY m.msg_index ASC LIMIT 1), '')
		FROM session s
		WHERE s.agent = ? AND s.parent_id = ?
		ORDER BY s.created_at ASC
	`, agent, sessionID)
	if err == nil {
		defer childRows.Close()
		for childRows.Next() {
			var child SessionLink
			child.Agent = agentDisplayName(agent)
			if err := childRows.Scan(&child.ID, &child.Title, &child.Model); err != nil {
				continue
			}
			stats, _ := st.SessionStats(ctx, agent, child.ID)
			child.Steps = stats.MessageCount
			child.TotalCost = stats.TotalCost
			child.TotalInput = stats.InputTokens
			child.TotalOutput = stats.OutputTokens
			child.TotalCacheRead = stats.CacheRead
			child.TotalCacheWrite = stats.CacheWrite
			if stats.InputTokens+stats.CacheRead > 0 {
				child.CacheHitRate = float64(stats.CacheRead) / float64(stats.InputTokens+stats.CacheRead) * 100
			}
			detail.Children = append(detail.Children, child)
		}
	}

	// parent
	if sess.ParentID != "" {
		var parentTitle, parentModel string
		err := st.DB().QueryRowContext(ctx, `
			SELECT COALESCE(s.project, ''),
			       COALESCE((SELECT m.model FROM message m
			                 WHERE m.agent = s.agent AND m.session_id = s.session_id
			                 AND m.role = 'assistant' ORDER BY m.msg_index ASC LIMIT 1), '')
			FROM session s
			WHERE s.agent = ? AND s.session_id = ?
		`, agent, sess.ParentID).Scan(&parentTitle, &parentModel)
		if err == nil {
			detail.Parent = &SessionLink{Agent: agentDisplayName(agent), ID: sess.ParentID, Title: parentTitle, Model: parentModel, Project: parentTitle}
		}
	}

	fillSessionStats(detail)
	return detail, nil
}

// sessionRevisionFromStore returns a revision hash for change detection.
// Hashes (last_activity, message_count, tool_count) — these change when the
// session is modified.
func sessionRevisionFromStore(ctx context.Context, agent, sessionID string) (string, error) {
	st := getTokeneksStore()
	if st == nil {
		return "", fmt.Errorf("store not open")
	}
	agent = strings.ToLower(agent)
	sess, err := st.GetSession(ctx, agent, sessionID)
	if err != nil {
		return "", err
	}
	stats, err := st.SessionStats(ctx, agent, sessionID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%d-%d", sess.LastActivity, stats.MessageCount, stats.ToolCallCount), nil
}

// CLISession is a store-backed session summary used by the CLI commands
// (oc/pi/claude list/detail, total). It carries enough pre-aggregated data
// to format the per-agent tables without re-reading the source.
type CLISession struct {
	Agent        string
	ID           string
	Title        string
	Project      string
	CreatedAt    int64
	LastActivity int64
	Model        string
	Provider     string
	ParentID     string
	StepCount    int
	Cost         float64
	TokensIn     int
	TokensOut    int
	CacheRead    int
	CacheWrite   int
	Steps        []compute.StepData // per-step tokens for ideal-cost compute
}

// aggregateSessionsFromStore returns CLI-ready session summaries for one agent,
// optionally filtered by date (YYYY-MM-DD) or by a days window.
// If date is non-empty it takes precedence over days.
// Sessions with no messages are excluded — the watcher keeps them in the
// store purely as a mtime-filter baseline (so it doesn't re-parse them on
// every poll), but they're never meant to surface in any list.
func aggregateSessionsFromStore(ctx context.Context, agent string, days int, date string) ([]CLISession, error) {
	st := getTokeneksStore()
	if st == nil {
		return nil, fmt.Errorf("store not open")
	}

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	var where string
	var args []any
	if date != "" {
		where = `WHERE agent = ? AND date(last_activity / 1000, 'unixepoch') = ?
		          AND EXISTS (SELECT 1 FROM message m
		                      WHERE m.agent = s.agent AND m.session_id = s.session_id)`
		args = []any{agent, date}
	} else {
		where = `WHERE agent = ? AND last_activity >= ?
		          AND EXISTS (SELECT 1 FROM message m
		                      WHERE m.agent = s.agent AND m.session_id = s.session_id)`
		args = []any{agent, cutoff}
	}

	rows, err := st.DB().QueryContext(ctx, `
		SELECT s.session_id, COALESCE(s.project, ''), COALESCE(s.parent_id, ''),
		       s.created_at, s.last_activity,
		       (SELECT m.model FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant' ORDER BY m.msg_index ASC LIMIT 1) AS model,
		       COALESCE((SELECT m.provider FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant' ORDER BY m.msg_index ASC LIMIT 1), '') AS provider,
		       COALESCE((SELECT SUM(m.input_tokens)  FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       COALESCE((SELECT SUM(m.output_tokens) FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       COALESCE((SELECT SUM(m.cache_read)    FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       COALESCE((SELECT SUM(m.cache_write)   FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       COALESCE((SELECT SUM(m.cost)          FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       (SELECT COUNT(*) FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant')
		FROM session s `+where+` ORDER BY s.created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []CLISession{}
	for rows.Next() {
		var s CLISession
		var model, provider *string
		if err := rows.Scan(&s.ID, &s.Title, &s.ParentID, &s.CreatedAt, &s.LastActivity, &model, &provider, &s.TokensIn, &s.TokensOut, &s.CacheRead, &s.CacheWrite, &s.Cost, &s.StepCount); err != nil {
			return nil, err
		}
		if model != nil {
			s.Model = *model
		}
		if provider != nil {
			s.Provider = *provider
		}
		s.Agent = agentDisplayName(agent)

		// fetch step data for ideal-cost calculation
		stepRows, err := st.DB().QueryContext(ctx, `
			SELECT input_tokens, cache_read, cache_write, output_tokens
			FROM message WHERE agent = ? AND session_id = ? AND role = 'assistant'
			ORDER BY msg_index ASC
		`, agent, s.ID)
		if err != nil {
			return nil, err
		}
		for stepRows.Next() {
			var in, cr, cw, o int
			if err := stepRows.Scan(&in, &cr, &cw, &o); err != nil {
				stepRows.Close()
				return nil, err
			}
			s.Steps = append(s.Steps, compute.StepData{Input: in, CacheRead: cr, CacheCreation: cw, Output: o})
		}
		stepRows.Close()
		out = append(out, s)
	}
	return out, rows.Err()
}

// ensureStoreReady makes sure the store has data for the given agent.
// If the store is empty for that agent, a one-shot sync is performed.
// Pass empty string to sync all agents.
func ensureStoreReady(agent string) error {
	st := getTokeneksStore()
	if st == nil {
		var err error
		st, err = openTokeneksStore()
		if err != nil {
			return err
		}
		setTokeneksStore(st)
	}
	if agent != "" {
		n, _ := st.CountSessions(context.Background(), agent)
		if n > 0 {
			return nil
		}
	} else {
		n, _ := st.CountSessions(context.Background(), "")
		if n > 0 {
			return nil
		}
	}
	sources, parsers := buildAgentIO()
	ing := &ingest.Ingestor{
		Store:     st,
		Agents:    []string{"claude", "pi", "opencode"},
		SourceFor: sources,
		ParserFor: parsers,
	}
	_, err := ing.Sync(context.Background())
	return err
}

// avoid "imported and not used" if the file changes
var _ = sql.ErrNoRows
