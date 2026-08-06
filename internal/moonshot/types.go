// Package moonshot speaks the OpenAI-compatible chat-completions API that
// Moonshot AI (Kimi) exposes.
package moonshot

// Message is one entry of the conversation sent upstream.
//
// Content has no omitempty: tool-role messages require the key even when
// empty, and the struct tag can't vary that by role.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`

	// FinishReason records why the API ended this turn (e.g. "stop",
	// "length", "tool_calls"). Stream sets it on the message it returns;
	// it is never sent upstream, so it stays out of the wire format.
	FinishReason string `json:"-"`

	// Usage reports the token cost of the request/response cycle that
	// produced this message. Stream sets it only when the upstream actually
	// reported usage; nil means unknown, not zero. Never sent upstream.
	Usage *Usage `json:"-"`
}

// Usage is the token accounting for one API call. CachedTokens is the subset
// of PromptTokens served from Moonshot's automatic prefix cache, billed at a
// fraction of the uncached rate.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
}

// ToolCall is a function call the model asked for.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc carries the called function's name and raw JSON arguments.
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef declares a callable tool to the model.
type ToolDef struct {
	Type     string      `json:"type"`
	Function ToolDefFunc `json:"function"`
}

// ToolDefFunc is a tool's name, description and JSON-Schema parameters.
type ToolDefFunc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}
