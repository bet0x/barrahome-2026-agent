package moonshot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer returns a server emitting the given SSE data lines.
func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want bearer test-key", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestStreamAssemblesText(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"Hola"}}]}`,
		`{"choices":[{"delta":{"content":", que"}}]}`,
		`{"choices":[{"delta":{"content":" tal"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", DefaultModel, srv.Client())

	var streamed strings.Builder
	msg, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "hola"}}, nil, 512,
		func(chunk string) error {
			streamed.WriteString(chunk)
			return nil
		})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if want := "Hola, que tal"; msg.Content != want {
		t.Errorf("assembled content = %q, want %q", msg.Content, want)
	}
	if streamed.String() != "Hola, que tal" {
		t.Errorf("streamed deltas = %q, want the same text incrementally", streamed.String())
	}
	if msg.Role != "assistant" {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
}

func TestStreamAssemblesToolCalls(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"cv.md\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", DefaultModel, srv.Client())
	msg, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "read my cv"}}, nil, 512, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(msg.ToolCalls), msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" {
		t.Errorf("tool call identity = %+v", tc)
	}
	if want := `{"path":"cv.md"}`; tc.Function.Arguments != want {
		t.Errorf("arguments = %q, want %q (fragments must be concatenated)", tc.Function.Arguments, want)
	}
}

// TestStreamCapturesFinishReason guards the root cause of a silent-truncation
// bug: finish_reason was parsed off the wire but never surfaced, so callers
// couldn't tell a length cutoff from a normal stop.
func TestStreamCapturesFinishReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{"stop", "stop"},
		{"length", "length"},
		{"tool_calls", "tool_calls"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := sseServer(t, []string{
				`{"choices":[{"delta":{"role":"assistant","content":"partial"}}]}`,
				fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":%q}]}`, tt.reason),
			})
			defer srv.Close()

			c := NewClient(srv.URL, "test-key", DefaultModel, srv.Client())
			msg, err := c.Stream(context.Background(),
				[]Message{{Role: "user", Content: "hola"}}, nil, 512, nil)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if msg.FinishReason != tt.reason {
				t.Errorf("FinishReason = %q, want %q", msg.FinishReason, tt.reason)
			}
		})
	}
}

// TestStreamHandlesLineSplitAcrossReads guards against a data: line arriving
// in more than one TCP read: the handler flushes mid-line, forcing the
// client's scanner to buffer a partial line before it sees the newline.
func TestStreamHandlesLineSplitAcrossReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":`)
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		fmt.Fprint(w, `"Hola"}}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", DefaultModel, srv.Client())
	msg, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "hola"}}, nil, 512, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if msg.Content != "Hola" {
		t.Errorf("content = %q, want %q (line split across reads must reassemble)", msg.Content, "Hola")
	}
}

// TestStreamIgnoresExtraChoices guards against a chunk that (unexpectedly,
// since we never request n>1) carries more than one choice: only choice 0 is
// ours, and a stray second choice must not be merged into the same text.
func TestStreamIgnoresExtraChoices(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"real"}},{"index":1,"delta":{"content":"OTHER-COMPLETION"}}]}`,
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", DefaultModel, srv.Client())
	msg, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "hola"}}, nil, 512, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if msg.Content != "real" {
		t.Errorf("content = %q, want %q (second choice must be ignored)", msg.Content, "real")
	}
}

// TestStreamIgnoresKeepAliveComments guards against SSE comment lines
// (":" prefix, commonly used as keep-alive pings) breaking the parse.
func TestStreamIgnoresKeepAliveComments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"Hola"}}]}`+"\n\n")
		fmt.Fprint(w, ": ping\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", DefaultModel, srv.Client())
	msg, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "hola"}}, nil, 512, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if msg.Content != "Hola" {
		t.Errorf("content = %q, want %q (comment lines must be skipped)", msg.Content, "Hola")
	}
}

// TestStreamErrorsWhenClosedWithoutDone guards against an upstream that ends
// the response cleanly (no dropped connection, so scanner.Err() is nil) but
// never sends [DONE]: without this check, a truncated tool-call argument
// would be silently handed to a tool as malformed JSON.
func TestStreamErrorsWhenClosedWithoutDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`+"\n\n")
		// No finish_reason chunk, no [DONE]: the handler just returns.
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", DefaultModel, srv.Client())
	if _, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "read my cv"}}, nil, 512, nil); err == nil {
		t.Fatal("Stream should error when the connection ends before [DONE]")
	}
}

func TestStreamReportsUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", DefaultModel, srv.Client())
	if _, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "x"}}, nil, 512, nil); err == nil {
		t.Fatal("Stream should return an error on HTTP 429")
	} else if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v, want it to mention the status code", err)
	}
}
