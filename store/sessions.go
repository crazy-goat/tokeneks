package store

import (
	"context"
	"database/sql"
	"fmt"
)

// UpsertSession inserts or replaces a session row.
// ON DELETE CASCADE on the messages table handles the children.
func (s *Store) UpsertSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session (agent, session_id, project, parent_id, created_at, last_activity, source_mtime)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent, session_id) DO UPDATE SET
			project       = excluded.project,
			parent_id     = excluded.parent_id,
			created_at    = excluded.created_at,
			last_activity = excluded.last_activity,
			source_mtime  = excluded.source_mtime
	`, sess.Agent, sess.SessionID, sess.Project, sess.ParentID, sess.CreatedAt, sess.LastActivity, sess.SourceMTime)
	if err != nil {
		return fmt.Errorf("UpsertSession(%s/%s): %w", sess.Agent, sess.SessionID, err)
	}
	return nil
}

// DeleteSession removes a session and cascades to its messages/tool_calls.
func (s *Store) DeleteSession(ctx context.Context, agent, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM session WHERE agent = ? AND session_id = ?`,
		agent, sessionID)
	return err
}

// SessionFilter narrows GetSessions results.
type SessionFilter struct {
	Agent        string // empty = all agents
	MinCreatedAt int64  // ms epoch; 0 = no lower bound
	Limit        int    // 0 = no limit
}

// GetSessions returns sessions matching the filter, ordered by last_activity desc.
func (s *Store) GetSessions(ctx context.Context, f SessionFilter) ([]Session, error) {
	q := `SELECT agent, session_id, project, parent_id, created_at, last_activity, source_mtime
	      FROM session WHERE 1=1`
	var args []any
	if f.Agent != "" {
		q += ` AND agent = ?`
		args = append(args, f.Agent)
	}
	if f.MinCreatedAt > 0 {
		q += ` AND last_activity >= ?`
		args = append(args, f.MinCreatedAt)
	}
	q += ` ORDER BY last_activity DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.Agent, &sess.SessionID, &sess.Project, &sess.ParentID, &sess.CreatedAt, &sess.LastActivity, &sess.SourceMTime); err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// GetSession returns one session or sql.ErrNoRows.
func (s *Store) GetSession(ctx context.Context, agent, sessionID string) (Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx, `
		SELECT agent, session_id, project, parent_id, created_at, last_activity, source_mtime
		FROM session WHERE agent = ? AND session_id = ?
	`, agent, sessionID).Scan(&sess.Agent, &sess.SessionID, &sess.Project, &sess.ParentID, &sess.CreatedAt, &sess.LastActivity, &sess.SourceMTime)
	return sess, err
}

// IngestSession replaces a session and all its messages in one transaction.
// Old messages and tool_calls are deleted first (CASCADE handles tool_calls).
func (s *Store) IngestSession(ctx context.Context, ps ParsedSession) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session WHERE agent = ? AND session_id = ?`,
		ps.Session.Agent, ps.Session.SessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session (agent, session_id, project, parent_id, created_at, last_activity, source_mtime)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, ps.Session.Agent, ps.Session.SessionID, ps.Session.Project, ps.Session.ParentID,
		ps.Session.CreatedAt, ps.Session.LastActivity, ps.Session.SourceMTime); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO message
		  (agent, session_id, msg_index, role, content, model, provider,
		   input_tokens, output_tokens, cache_read, cache_write, cost,
		   stop_reason, thinking, response, tool_call_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	tcStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO tool_call (message_id, call_id, name, input, error, status, duration_ms)
		VALUES (?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer tcStmt.Close()

	for _, pm := range ps.Messages {
		res, err := stmt.ExecContext(ctx,
			pm.Message.Agent, pm.Message.SessionID, pm.Message.MsgIndex, pm.Message.Role,
			pm.Message.Content, pm.Message.Model, pm.Message.Provider,
			pm.Message.InputTokens, pm.Message.OutputTokens, pm.Message.CacheRead, pm.Message.CacheWrite,
			pm.Message.Cost, pm.Message.StopReason, pm.Message.Thinking, pm.Message.Response,
			pm.Message.ToolCallID, pm.Message.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert message idx=%d: %w", pm.Message.MsgIndex, err)
		}
		msgID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for _, tc := range pm.ToolCalls {
			if _, err := tcStmt.ExecContext(ctx,
				msgID, tc.CallID, tc.Name, tc.Input, tc.Error, tc.Status, tc.DurationMs); err != nil {
				return fmt.Errorf("insert tool_call: %w", err)
			}
		}
	}
	return tx.Commit()
}

// SessionStats holds aggregated counters for one session.
type SessionStats struct {
	SessionID     string
	MessageCount  int
	ToolCallCount int
	ToolErrors    int
	TotalCost     float64
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheWrite    int
}

// SessionStats returns aggregated counters for a session in one query.
func (s *Store) SessionStats(ctx context.Context, agent, sessionID string) (SessionStats, error) {
	var st SessionStats
	st.SessionID = sessionID
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM message WHERE agent = ? AND session_id = ?),
		  COALESCE((SELECT SUM(input_tokens)  FROM message WHERE agent = ? AND session_id = ? AND role = 'assistant'), 0),
		  COALESCE((SELECT SUM(output_tokens) FROM message WHERE agent = ? AND session_id = ? AND role = 'assistant'), 0),
		  COALESCE((SELECT SUM(cache_read)    FROM message WHERE agent = ? AND session_id = ? AND role = 'assistant'), 0),
		  COALESCE((SELECT SUM(cache_write)   FROM message WHERE agent = ? AND session_id = ? AND role = 'assistant'), 0),
		  COALESCE((SELECT SUM(cost)          FROM message WHERE agent = ? AND session_id = ? AND role = 'assistant'), 0)
	`, agent, sessionID, agent, sessionID, agent, sessionID, agent, sessionID, agent, sessionID, agent, sessionID).
		Scan(&st.MessageCount, &st.InputTokens, &st.OutputTokens, &st.CacheRead, &st.CacheWrite, &st.TotalCost)
	if err != nil {
		return st, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(error), 0)
		FROM tool_call tc
		JOIN message m ON m.id = tc.message_id
		WHERE m.agent = ? AND m.session_id = ?
	`, agent, sessionID).Scan(&st.ToolCallCount, &st.ToolErrors)
	return st, err
}

// CountSessions returns the number of sessions for an agent (empty = all).
func (s *Store) CountSessions(ctx context.Context, agent string) (int, error) {
	q := `SELECT COUNT(*) FROM session`
	var args []any
	if agent != "" {
		q += ` WHERE agent = ?`
		args = append(args, agent)
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// ensure sql is referenced for the build (in case IngestSession moves elsewhere)
var _ = sql.ErrNoRows
