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

If a question is not about this site's content, answer from your own
knowledge directly instead of searching. If one or two searches turn up
nothing relevant, say so plainly rather than trying more searches.

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

// Event kinds streamed to the browser. Unknown kinds are meant to be ignored
// by older frontends, so adding one is backward compatible.
const (
	EventText       EventKind = "text"
	EventToolStart  EventKind = "tool_start"
	EventToolResult EventKind = "tool_result"
	EventTruncated  EventKind = "truncated"
)

// truncationNotice tells the visitor an answer was cut off by max_tokens,
// rather than leaving a mid-word stop looking like a broken agent.
const truncationNotice = "[response was cut short at the length limit]"

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
	// OnUsage, if set, is called once per Run with the token cost summed
	// across every model call the turn made (including tool rounds). It is
	// operational data — never sent to the visitor.
	OnUsage func(moonshot.Usage)
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
//
// If the model is still requesting tools once MaxToolRounds is spent, Run
// makes one last call with no tools offered, forcing a prose answer from
// whatever context was already gathered, rather than failing the turn.
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

	// total is reported however the turn ends, including on error: tokens
	// already spent are a real cost regardless of whether the turn succeeded.
	var total moonshot.Usage
	defer func() {
		if deps.OnUsage != nil && total.TotalTokens > 0 {
			deps.OnUsage(total)
		}
	}()

	convo := append([]moonshot.Message(nil), history...)
	convo = append(convo, moonshot.Message{Role: "user", Content: userMessage})

	for round := 0; round < deps.MaxToolRounds; round++ {
		assistant, err := streamTurn(ctx, deps, convo, ToolSchemas(), emit)
		if err != nil {
			return nil, err
		}
		addUsage(&total, assistant.Usage)
		convo = append(convo, *assistant)

		if len(assistant.ToolCalls) == 0 {
			if err := emitTruncationNotice(assistant, emit); err != nil {
				return nil, err
			}
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

	assistant, err := streamTurn(ctx, deps, convo, nil, emit)
	if err != nil {
		return nil, err
	}
	addUsage(&total, assistant.Usage)
	if err := emitTruncationNotice(assistant, emit); err != nil {
		return nil, err
	}
	return append(convo, *assistant), nil
}

// addUsage folds u into total; u is nil when the upstream reported no usage
// for that call, which must not be counted as zero tokens spent.
func addUsage(total *moonshot.Usage, u *moonshot.Usage) {
	if u == nil {
		return
	}
	total.PromptTokens += u.PromptTokens
	total.CompletionTokens += u.CompletionTokens
	total.TotalTokens += u.TotalTokens
	total.CachedTokens += u.CachedTokens
}

// emitTruncationNotice tells the visitor when a turn ended because max_tokens
// was reached, rather than letting a mid-word cutoff read as a broken agent.
// It does not retry or continue the generation: that would multiply cost on
// exactly the responses that are already the most expensive.
func emitTruncationNotice(assistant *moonshot.Message, emit Emit) error {
	if assistant.FinishReason != "length" {
		return nil
	}
	return emit(Event{Kind: EventTruncated, Text: truncationNotice})
}

// streamTurn sends convo upstream with a fresh system prompt and the given
// tools (nil to force a prose-only answer), streaming text deltas through emit.
func streamTurn(
	ctx context.Context,
	deps Deps,
	convo []moonshot.Message,
	tools []moonshot.ToolDef,
	emit Emit,
) (*moonshot.Message, error) {
	system := SystemPrompt
	if deps.MaxTokens > 0 {
		// The truncation notice is a fallback for when this doesn't work, not
		// a substitute for it: telling the model its ceiling lets it wrap up
		// on its own instead of being cut off mid-sentence.
		system += fmt.Sprintf("\nYour reply is capped at %d tokens; finish within that budget.", deps.MaxTokens)
	}
	upstream := append([]moonshot.Message{{Role: "system", Content: system}}, convo...)
	return deps.Model.Stream(ctx, upstream, tools, deps.MaxTokens,
		func(chunk string) error {
			return emit(Event{Kind: EventText, Text: chunk})
		})
}
