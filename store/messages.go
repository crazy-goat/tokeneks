package store

import "context"

// GetMessages returns all messages for a session, ordered by msg_index.
func (s *Store) GetMessages(ctx context.Context, agent, sessionID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, session_id, msg_index, role, content, model, provider,
		       input_tokens, output_tokens, cache_read, cache_write, cost,
		       stop_reason, thinking, response, tool_call_id, created_at
		FROM message
		WHERE agent = ? AND session_id = ?
		ORDER BY msg_index ASC
	`, agent, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Agent, &m.SessionID, &m.MsgIndex, &m.Role, &m.Content,
			&m.Model, &m.Provider, &m.InputTokens, &m.OutputTokens, &m.CacheRead, &m.CacheWrite,
			&m.Cost, &m.StopReason, &m.Thinking, &m.Response, &m.ToolCallID, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetToolCalls returns all tool calls for a message, in insertion order.
func (s *Store) GetToolCalls(ctx context.Context, messageID int64) ([]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, message_id, call_id, name, input, error, status, duration_ms
		FROM tool_call WHERE message_id = ? ORDER BY id ASC
	`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var tc ToolCall
		if err := rows.Scan(&tc.ID, &tc.MessageID, &tc.CallID, &tc.Name, &tc.Input,
			&tc.Error, &tc.Status, &tc.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}
