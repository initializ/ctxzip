package ctxzip

// Role constants for Message. ctxzip uses plain strings (not a typed enum) so
// adapters can map provider/runtime roles in without conversion friction.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is ctxzip's provider-agnostic message. A forge adapter maps
// llm.ChatMessage to and from this type; the engine deliberately knows nothing
// about any runtime's native struct.
type Message struct {
	Role string
	// Content is the text body. Tool outputs — the prime compression target —
	// arrive as Role==RoleTool messages.
	Content string
	// Name and ToolCallID are carried through untouched for tool messages.
	Name       string
	ToolCallID string
}

// Transform records what happened to one message during compression.
type Transform struct {
	Index        int
	Role         string
	Strategy     string
	TokensBefore int
	TokensAfter  int
	Markers      []string
}

// Result is the output of Compress.
type Result struct {
	// Messages is the compressed slice, same length and order as the input.
	Messages []Message
	// TokensBefore/After are approximate counts over all message content.
	TokensBefore int
	TokensAfter  int
	// Transforms lists the messages that were actually changed.
	Transforms []Transform
}

// SavedTokens is the approximate token reduction.
func (r *Result) SavedTokens() int {
	if r.TokensBefore <= r.TokensAfter {
		return 0
	}
	return r.TokensBefore - r.TokensAfter
}

// Ratio is the fraction of tokens removed, in [0,1].
func (r *Result) Ratio() float64 {
	if r.TokensBefore == 0 {
		return 0
	}
	return float64(r.SavedTokens()) / float64(r.TokensBefore)
}
