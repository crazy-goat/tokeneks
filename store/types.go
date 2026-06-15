package store

// Roles used in message.role.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

// Session is one agent session.
type Session struct {
	Agent        string
	SessionID    string
	Project      string
	ParentID     string
	CreatedAt    int64
	LastActivity int64
	SourceMTime  int64 // ms epoch of the underlying source file/row; used for incremental sync
}

// Message is one event in a session.
// For role=assistant, Model/Provider/InputTokens/.../Cost/StopReason/Thinking/Response may be set.
// For role=tool, ToolCallID links back to the tool_call.call_id that produced it.
type Message struct {
	ID           int64
	Agent        string
	SessionID    string
	MsgIndex     int
	Role         string
	Content      string
	Model        string
	Provider     string
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
	Cost         float64
	StopReason   string
	Thinking     string
	Response     string
	ToolCallID   string
	CreatedAt    int64
}

// ToolCall is one tool invocation issued by an assistant message.
// Output is not stored here — the corresponding role=tool message
// (linked by call_id) holds it.
type ToolCall struct {
	ID         int64
	MessageID  int64
	CallID     string
	Name       string
	Input      string
	Error      bool
	Status     string
	DurationMs int64
}

// ParsedSession is the in-memory shape produced by a parser and
// consumed by Ingestor. ToolCalls are nested under their parent
// message; Ingestor resolves MessageID during insert.
type ParsedSession struct {
	Session  Session
	Messages []ParsedMessage
}

// ParsedMessage groups a Message with the tool calls that were
// issued as part of it.
type ParsedMessage struct {
	Message   Message
	ToolCalls []ToolCall
}
