// Package moonshot speaks the OpenAI-compatible chat-completions API that
// Moonshot AI (Kimi) exposes.
package moonshot

// Message is one entry of the conversation sent upstream.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
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
