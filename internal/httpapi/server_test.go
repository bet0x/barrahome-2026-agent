package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bet0x/barrahome-2026-agent/internal/config"
	"github.com/bet0x/barrahome-2026-agent/internal/limits"
	"github.com/bet0x/barrahome-2026-agent/internal/moonshot"
	"github.com/bet0x/barrahome-2026-agent/internal/session"
)

type stubModel struct{ reply string }

func (s *stubModel) Stream(_ context.Context, _ []moonshot.Message,
	_ []moonshot.ToolDef, _ int, onText func(string) error) (*moonshot.Message, error) {
	if onText != nil {
		if err := onText(s.reply); err != nil {
			return nil, err
		}
	}
	return &moonshot.Message{Role: "assistant", Content: s.reply}, nil
}

// blockingModel lets a test hold a request open until it chooses to release
// it, so it can drive a second request into the same in-flight session.
type blockingModel struct {
	started chan struct{}
	proceed chan struct{}
}

func (m *blockingModel) Stream(_ context.Context, _ []moonshot.Message,
	_ []moonshot.ToolDef, _ int, onText func(string) error) (*moonshot.Message, error) {
	close(m.started)
	<-m.proceed
	if onText != nil {
		if err := onText("done waiting"); err != nil {
			return nil, err
		}
	}
	return &moonshot.Message{Role: "assistant", Content: "done waiting"}, nil
}

type stubTools struct{}

func (stubTools) Call(string, string) string { return "unused" }

func newTestDeps(t *testing.T, perIPPerHour, maxConcurrent int) (Deps, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		AllowedOrigins: []string{"https://barrahome.org"},
		MaxTurns:       20,
		MaxTokens:      256,
		MaxToolRounds:  4,
		SessionTTL:     30 * time.Minute,
	}
	return Deps{
		Cfg:      cfg,
		Tools:    stubTools{},
		Model:    &stubModel{reply: "hola desde el agente"},
		Sessions: session.NewStore(session.Config{TTL: cfg.SessionTTL, MaxTurns: cfg.MaxTurns}),
		Limiter:  limits.NewLimiter(perIPPerHour, maxConcurrent),
	}, cfg
}

func newTestServer(t *testing.T, perIPPerHour, maxConcurrent int) http.Handler {
	t.Helper()
	d, _ := newTestDeps(t, perIPPerHour, maxConcurrent)
	return NewServer(d)
}

func post(t *testing.T, h http.Handler, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.RemoteAddr = "203.0.113.9:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStreamHappyPath(t *testing.T) {
	h := newTestServer(t, 20, 10)
	rec := post(t, h, "https://barrahome.org", `{"session_id":"s1","message":"hola"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hola desde el agente") {
		t.Errorf("body missing the streamed text: %q", body)
	}
	if !strings.Contains(body, "event: done") {
		t.Errorf("body should end with a done event: %q", body)
	}
}

func TestStreamRejectsForeignOrigin(t *testing.T) {
	h := newTestServer(t, 20, 10)
	rec := post(t, h, "https://evil.example", `{"session_id":"s1","message":"hola"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestStreamRejectsBadRequests(t *testing.T) {
	h := newTestServer(t, 20, 10)

	if rec := post(t, h, "https://barrahome.org", `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON status = %d, want 400", rec.Code)
	}
	if rec := post(t, h, "https://barrahome.org", `{"session_id":"","message":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing session_id status = %d, want 400", rec.Code)
	}
	if rec := post(t, h, "https://barrahome.org", `{"session_id":"s1","message":"   "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("blank message status = %d, want 400", rec.Code)
	}
}

func TestStreamEnforcesPerIPQuota(t *testing.T) {
	h := newTestServer(t, 1, 10)
	if rec := post(t, h, "https://barrahome.org", `{"session_id":"s1","message":"one"}`); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec.Code)
	}
	rec := post(t, h, "https://barrahome.org", `{"session_id":"s2","message":"two"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request status = %d, want 429", rec.Code)
	}
}

// TestStreamRejectsConcurrentSameSession exercises the Checkout single-flight
// guarantee end-to-end: a second request for a session already mid-stream
// must be refused with 409, not queued behind the first.
func TestStreamRejectsConcurrentSameSession(t *testing.T) {
	d, _ := newTestDeps(t, 20, 10)
	model := &blockingModel{started: make(chan struct{}), proceed: make(chan struct{})}
	d.Model = model
	h := NewServer(d)

	var wg sync.WaitGroup
	var firstRec *httptest.ResponseRecorder
	wg.Add(1)
	go func() {
		defer wg.Done()
		firstRec = post(t, h, "https://barrahome.org", `{"session_id":"busy","message":"one"}`)
	}()

	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the model")
	}

	rec := post(t, h, "https://barrahome.org", `{"session_id":"busy","message":"two"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("concurrent request status = %d, want 409", rec.Code)
	}

	close(model.proceed)
	wg.Wait()
	if firstRec.Code != http.StatusOK {
		t.Errorf("first request status = %d, want 200", firstRec.Code)
	}
}

func TestHealthz(t *testing.T) {
	h := newTestServer(t, 20, 10)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", rec.Code)
	}
}
