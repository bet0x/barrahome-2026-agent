package moonshot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Defaults for the Moonshot (Kimi) endpoint. kimi-k2.6 is the cheapest tier
// that supports streaming plus tool calls.
const (
	DefaultBaseURL = "https://api.moonshot.ai/v1"
	DefaultModel   = "kimi-k2.6"
)

// Client calls Moonshot's OpenAI-compatible chat-completions endpoint.
type Client struct {
	baseURL         string
	apiKey          string
	model           string
	thinking        string // "enabled", "disabled", or "" to omit the field
	reasoningEffort string // "low", "high", "max", or "" to omit the field
	hc              *http.Client
}

// NewClient returns a client. hc may be nil, in which case http.DefaultClient
// is used. thinking and reasoningEffort are sent verbatim when non-empty;
// config.Load decides which one (if either) applies to the chosen model.
func NewClient(baseURL, apiKey, model, thinking, reasoningEffort string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          apiKey,
		model:           model,
		thinking:        thinking,
		reasoningEffort: reasoningEffort,
		hc:              hc,
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
	// MaxCompletionTokens caps the reply's length, not prompt+reply: max_tokens
	// is deprecated in favor of this field.
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Thinking            *thinkingOption `json:"thinking,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
}

// thinkingOption controls kimi-k2.6's extended thinking. Keep is left unset
// (defaults to null upstream); we have no use yet for retaining transcripts.
type thinkingOption struct {
	Type string `json:"type"`
}

// streamOptions requests the trailing usage chunk on a streamed response.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// rawUsage mirrors the wire shape of a usage object. CachedTokens rides
// under prompt_tokens_details, per Moonshot's documented cache accounting.
type rawUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// streamChunk mirrors one SSE payload. tool_calls arrive in fragments keyed by
// index, so arguments must be concatenated across chunks. Usage can arrive
// two ways: as a trailing chunk with empty choices (the documented shape for
// stream_options.include_usage), or nested in a choice alongside its
// finish_reason (observed from the live API). Both are read defensively.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string    `json:"finish_reason"`
		Usage        *rawUsage `json:"usage"`
	} `json:"choices"`
	Usage *rawUsage `json:"usage"`
}

// Stream sends msgs upstream and consumes the SSE response. Text deltas are
// passed to onText as they arrive (onText may be nil); the fully assembled
// assistant message is returned, including any tool calls the model requested.
func (c *Client) Stream(
	ctx context.Context,
	msgs []Message,
	tools []ToolDef,
	maxTokens int,
	onText func(string) error,
) (*Message, error) {
	reqBody := chatRequest{
		Model:               c.model,
		Messages:            msgs,
		Tools:               tools,
		Stream:              true,
		MaxCompletionTokens: maxTokens,
		StreamOptions:       &streamOptions{IncludeUsage: true},
	}
	if c.thinking != "" {
		reqBody.Thinking = &thinkingOption{Type: c.thinking}
	}
	if c.reasoningEffort != "" {
		reqBody.ReasoningEffort = c.reasoningEffort
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("moonshot: encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("moonshot: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moonshot: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("moonshot: upstream returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	out := &Message{Role: "assistant"}
	var text strings.Builder
	// Tool-call fragments accumulate per index before being ordered.
	type partialCall struct {
		id, name string
		args     strings.Builder
	}
	partials := map[int]*partialCall{}
	sawDone := false
	finishReason := ""
	var usage *rawUsage

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Blank lines separate SSE events; lines starting with ":" are
		// comments, commonly used as keep-alive pings. Neither carries data.
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // tolerate keep-alives and non-JSON comments
		}
		// The usage-only trailing chunk has an empty choices array, so this
		// must be read before the choices-empty skip below discards it.
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		// We never request n>1, so only the first choice is ours; a stray
		// second choice must not be merged into the same text/tool-call state.
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
		if choice.Usage != nil {
			usage = choice.Usage
		}
		if choice.Delta.Content != "" {
			text.WriteString(choice.Delta.Content)
			if onText != nil {
				if err := onText(choice.Delta.Content); err != nil {
					return nil, err
				}
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			p, ok := partials[tc.Index]
			if !ok {
				p = &partialCall{}
				partials[tc.Index] = p
			}
			if tc.ID != "" {
				p.id = tc.ID
			}
			if tc.Function.Name != "" {
				p.name = tc.Function.Name
			}
			p.args.WriteString(tc.Function.Arguments)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("moonshot: reading stream: %w", err)
	}
	if !sawDone {
		// The connection ended without the [DONE] sentinel: whatever was
		// assembled may be a partial message, and a truncated tool-call
		// argument is malformed JSON waiting to be handed to a tool.
		return nil, fmt.Errorf("moonshot: stream ended before [DONE]")
	}

	out.Content = text.String()
	out.FinishReason = finishReason
	if usage != nil {
		out.Usage = &Usage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			CachedTokens:     usage.PromptTokensDetails.CachedTokens,
		}
	}

	indexes := make([]int, 0, len(partials))
	for i := range partials {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		p := partials[i]
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:   p.id,
			Type: "function",
			Function: ToolCallFunc{
				Name:      p.name,
				Arguments: p.args.String(),
			},
		})
	}
	return out, nil
}
