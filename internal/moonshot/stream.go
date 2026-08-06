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
	baseURL string
	apiKey  string
	model   string
	hc      *http.Client
}

// NewClient returns a client. hc may be nil, in which case http.DefaultClient
// is used.
func NewClient(baseURL, apiKey, model string, hc *http.Client) *Client {
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
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		hc:      hc,
	}
}

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Tools     []ToolDef `json:"tools,omitempty"`
	Stream    bool      `json:"stream"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

// streamChunk mirrors one SSE payload. tool_calls arrive in fragments keyed by
// index, so arguments must be concatenated across chunks.
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
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
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
	body, err := json.Marshal(chatRequest{
		Model:     c.model,
		Messages:  msgs,
		Tools:     tools,
		Stream:    true,
		MaxTokens: maxTokens,
	})
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
		if len(chunk.Choices) == 0 {
			continue
		}
		// We never request n>1, so only the first choice is ours; a stray
		// second choice must not be merged into the same text/tool-call state.
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
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
