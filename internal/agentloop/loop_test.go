package agentloop

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bet0x/barrahome-2026-agent/internal/moonshot"
)

// fakeModel replays scripted assistant turns and records what it was sent.
type fakeModel struct {
	turns []*moonshot.Message
	calls int
	seen  [][]moonshot.Message
}

func (f *fakeModel) Stream(_ context.Context, msgs []moonshot.Message,
	_ []moonshot.ToolDef, _ int, onText func(string) error) (*moonshot.Message, error) {
	f.seen = append(f.seen, append([]moonshot.Message(nil), msgs...))
	turn := f.turns[min(f.calls, len(f.turns)-1)]
	f.calls++
	if turn.Content != "" && onText != nil {
		if err := onText(turn.Content); err != nil {
			return nil, err
		}
	}
	return turn, nil
}

type fakeTools struct{ calls []string }

func (f *fakeTools) Call(name, args string) string {
	f.calls = append(f.calls, name+" "+args)
	return "TOOL_RESULT for " + name
}

func TestRunPlainAnswer(t *testing.T) {
	model := &fakeModel{turns: []*moonshot.Message{
		{Role: "assistant", Content: "Hola, soy el agente."},
	}}
	var events []Event
	history, err := Run(context.Background(),
		Deps{Model: model, Tools: &fakeTools{}, MaxTokens: 512, MaxToolRounds: 4},
		nil, "hola", func(e Event) error {
			events = append(events, e)
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// history must be user + assistant, ready to seed the next turn.
	if len(history) != 2 || history[0].Role != "user" || history[1].Role != "assistant" {
		t.Fatalf("history = %+v, want [user assistant]", history)
	}
	// The system prompt is sent upstream but not stored in history.
	if got := model.seen[0][0].Role; got != "system" {
		t.Errorf("first upstream message role = %q, want system", got)
	}
	var text strings.Builder
	for _, e := range events {
		if e.Kind == EventText {
			text.WriteString(e.Text)
		}
	}
	if text.String() != "Hola, soy el agente." {
		t.Errorf("streamed text = %q", text.String())
	}
}

func TestRunExecutesToolCallsAndFeedsResultsBack(t *testing.T) {
	model := &fakeModel{turns: []*moonshot.Message{
		{Role: "assistant", ToolCalls: []moonshot.ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: moonshot.ToolCallFunc{Name: "read_file", Arguments: `{"path":"cv.md"}`},
		}}},
		{Role: "assistant", Content: "Tu CV dice que eres SRE."},
	}}
	tools := &fakeTools{}
	var kinds []EventKind

	history, err := Run(context.Background(),
		Deps{Model: model, Tools: tools, MaxTokens: 512, MaxToolRounds: 4},
		nil, "que dice mi cv?", func(e Event) error {
			kinds = append(kinds, e.Kind)
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(tools.calls) != 1 || !strings.Contains(tools.calls[0], "read_file") {
		t.Errorf("tool calls = %v, want one read_file", tools.calls)
	}
	// The second upstream call must include the tool result.
	if len(model.seen) != 2 {
		t.Fatalf("model called %d times, want 2", len(model.seen))
	}
	last := model.seen[1]
	var sawToolResult bool
	for _, m := range last {
		if m.Role == "tool" && strings.Contains(m.Content, "TOOL_RESULT") {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Errorf("second request missing the tool result: %+v", last)
	}
	// Lifecycle events let the terminal show progress.
	var sawStart, sawResult bool
	for _, k := range kinds {
		switch k {
		case EventToolStart:
			sawStart = true
		case EventToolResult:
			sawResult = true
		}
	}
	if !sawStart || !sawResult {
		t.Errorf("event kinds = %v, want tool_start and tool_result", kinds)
	}
	if history[len(history)-1].Content != "Tu CV dice que eres SRE." {
		t.Errorf("final history entry = %+v", history[len(history)-1])
	}
}

func TestRunStopsAtMaxToolRounds(t *testing.T) {
	// A model that always asks for another tool call must not loop forever.
	model := &fakeModel{turns: []*moonshot.Message{{
		Role: "assistant",
		ToolCalls: []moonshot.ToolCall{{
			ID:       "call_x",
			Type:     "function",
			Function: moonshot.ToolCallFunc{Name: "list_dir", Arguments: `{"path":""}`},
		}},
	}}}
	tools := &fakeTools{}

	if _, err := Run(context.Background(),
		Deps{Model: model, Tools: tools, MaxTokens: 512, MaxToolRounds: 3},
		nil, "loop", func(Event) error { return nil }); err == nil {
		t.Fatal("Run should fail once MaxToolRounds is exhausted")
	}
	if len(tools.calls) > 3 {
		t.Errorf("tool executed %d times, want at most MaxToolRounds=3", len(tools.calls))
	}
}

// TestRunHandlesTextAndToolCallsInSameTurn guards against a model that
// narrates ("Let me check...") in the same turn it requests a tool call: the
// text must still stream and the tool call must still run and get answered.
func TestRunHandlesTextAndToolCallsInSameTurn(t *testing.T) {
	model := &fakeModel{turns: []*moonshot.Message{
		{
			Role:    "assistant",
			Content: "Let me check...",
			ToolCalls: []moonshot.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: moonshot.ToolCallFunc{Name: "read_file", Arguments: `{"path":"cv.md"}`},
			}},
		},
		{Role: "assistant", Content: "Tu CV dice que eres SRE."},
	}}
	tools := &fakeTools{}
	var text strings.Builder

	history, err := Run(context.Background(),
		Deps{Model: model, Tools: tools, MaxTokens: 512, MaxToolRounds: 4},
		nil, "que dice mi cv?", func(e Event) error {
			if e.Kind == EventText {
				text.WriteString(e.Text)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Both turns stream text; the first turn's narration must be part of it.
	if !strings.HasPrefix(text.String(), "Let me check...") {
		t.Errorf("streamed text = %q, want it to start with the first turn's narration", text.String())
	}
	if len(tools.calls) != 1 {
		t.Errorf("tool calls = %v, want the tool call to still run", tools.calls)
	}
	if history[len(history)-1].Content != "Tu CV dice que eres SRE." {
		t.Errorf("final history entry = %+v", history[len(history)-1])
	}
}

// TestRunAbortsPromptlyWhenEmitErrors guards the disconnected-browser path: a
// failing emit must stop the run immediately, without further model calls or
// tool executions.
func TestRunAbortsPromptlyWhenEmitErrors(t *testing.T) {
	disconnectErr := fmt.Errorf("client disconnected")
	model := &fakeModel{turns: []*moonshot.Message{
		{Role: "assistant", Content: "Hola, soy el agente."},
	}}
	tools := &fakeTools{}

	_, err := Run(context.Background(),
		Deps{Model: model, Tools: tools, MaxTokens: 512, MaxToolRounds: 4},
		nil, "hola", func(e Event) error {
			return disconnectErr
		})
	if err != disconnectErr {
		t.Fatalf("Run error = %v, want the emit error propagated", err)
	}
	if model.calls != 1 {
		t.Errorf("model called %d times, want exactly 1 (the failing call)", model.calls)
	}
	if len(tools.calls) != 0 {
		t.Errorf("tools called %v, want none after emit failed", tools.calls)
	}
}

func TestToolSchemasCoverAllThreeTools(t *testing.T) {
	names := map[string]bool{}
	for _, s := range ToolSchemas() {
		names[s.Function.Name] = true
		if s.Type != "function" {
			t.Errorf("schema %q has type %q, want function", s.Function.Name, s.Type)
		}
		if s.Function.Description == "" {
			t.Errorf("schema %q has no description", s.Function.Name)
		}
	}
	for _, want := range []string{"list_dir", "read_file", "search_content"} {
		if !names[want] {
			t.Errorf("ToolSchemas missing %q", want)
		}
	}
}
