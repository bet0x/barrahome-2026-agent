// Package agentloop orchestrates the conversation between the model and the
// read-only content tools.
package agentloop

import (
	"context"
	"fmt"

	"github.com/bet0x/barrahome-2026-agent/internal/moonshot"
)

// SystemPrompt tells the model what it is and when to reach for tools. General
// conversation is allowed; the tools exist for questions about the site.
const SystemPrompt = `You are the terminal assistant on barrahome.org, Alberto Ferrer's technical blog.

You may chat about general topics. When a question concerns this site — its
posts, the CV, the projects page — use your tools to read the actual content
instead of guessing, and say plainly when something is not there.

Tools available:
- list_dir(path): list a directory, relative to the content root ("" is the root)
- read_file(path): read a file, relative to the content root
- search_content(query): case-insensitive search across all content

Posts live under year/month/day directories, e.g. 2026/07/24/some-post.md.
Reply in the same language the visitor uses. Be concise and technical.`

// Streamer is the upstream model. *moonshot.Client satisfies it.
type Streamer interface {
	Stream(ctx context.Context, msgs []moonshot.Message, tools []moonshot.ToolDef,
		maxTokens int, onText func(string) error) (*moonshot.Message, error)
}

// ToolRunner executes a tool call. *content.Tools satisfies it.
type ToolRunner interface {
	Call(name, argumentsJSON string) string
}

// EventKind identifies what happened during a run.
type EventKind string

// Event kinds streamed to the browser.
const (
	EventText       EventKind = "text"
	EventToolStart  EventKind = "tool_start"
	EventToolResult EventKind = "tool_result"
)

// Event is one thing worth telling the frontend about.
type Event struct {
	Kind EventKind
	Text string // text delta, or a short tool description
}

// Emit delivers an event. Returning an error aborts the run, which is how a
// disconnected client stops work.
type Emit func(Event) error

// Deps are the run's collaborators and bounds.
type Deps struct {
	Model         Streamer
	Tools         ToolRunner
	MaxTokens     int
	MaxToolRounds int
}

// ToolSchemas declares the three read-only tools to the model.
func ToolSchemas() []moonshot.ToolDef {
	pathParam := func(desc string) any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": desc},
			},
			"required": []string{"path"},
		}
	}
	return []moonshot.ToolDef{
		{Type: "function", Function: moonshot.ToolDefFunc{
			Name:        "list_dir",
			Description: "List the entries of a directory in the site content. Use \"\" for the root. Directories end with /.",
			Parameters:  pathParam("Directory path relative to the content root, e.g. \"2026/07\" or \"\" for the root."),
		}},
		{Type: "function", Function: moonshot.ToolDefFunc{
			Name:        "read_file",
			Description: "Read a file from the site content. Large files are truncated.",
			Parameters:  pathParam("File path relative to the content root, e.g. \"cv.md\"."),
		}},
		{Type: "function", Function: moonshot.ToolDefFunc{
			Name:        "search_content",
			Description: "Case-insensitive search across all site content. Returns matching file paths with the matching line.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Text to search for."},
				},
				"required": []string{"query"},
			},
		}},
	}
}

// Run performs one visitor turn: it appends userMessage to history, calls the
// model, executes any tool calls it asks for, feeds the results back, and
// repeats until the model answers with text. It returns the updated history
// (without the system prompt, which is prepended fresh on each request).
func Run(
	ctx context.Context,
	deps Deps,
	history []moonshot.Message,
	userMessage string,
	emit Emit,
) ([]moonshot.Message, error) {
	if deps.MaxToolRounds <= 0 {
		deps.MaxToolRounds = 4
	}

	convo := append([]moonshot.Message(nil), history...)
	convo = append(convo, moonshot.Message{Role: "user", Content: userMessage})

	for round := 0; round < deps.MaxToolRounds; round++ {
		upstream := append([]moonshot.Message{{Role: "system", Content: SystemPrompt}}, convo...)

		assistant, err := deps.Model.Stream(ctx, upstream, ToolSchemas(), deps.MaxTokens,
			func(chunk string) error {
				return emit(Event{Kind: EventText, Text: chunk})
			})
		if err != nil {
			return nil, err
		}
		convo = append(convo, *assistant)

		if len(assistant.ToolCalls) == 0 {
			return convo, nil
		}

		for _, tc := range assistant.ToolCalls {
			if err := emit(Event{
				Kind: EventToolStart,
				Text: fmt.Sprintf("%s %s", tc.Function.Name, tc.Function.Arguments),
			}); err != nil {
				return nil, err
			}

			result := deps.Tools.Call(tc.Function.Name, tc.Function.Arguments)

			if err := emit(Event{Kind: EventToolResult, Text: tc.Function.Name}); err != nil {
				return nil, err
			}
			convo = append(convo, moonshot.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}
	return nil, fmt.Errorf("agentloop: gave up after %d tool rounds", deps.MaxToolRounds)
}
