package httpapi

import (
	"context"
	"fmt"
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

type stubModel struct {
	reply string
	usage *moonshot.Usage
}

func (s *stubModel) Stream(_ context.Context, _ []moonshot.Message,
	_ []moonshot.ToolDef, _ int, onText func(string) error) (*moonshot.Message, error) {
	if onText != nil {
		if err := onText(s.reply); err != nil {
			return nil, err
		}
	}
	return &moonshot.Message{Role: "assistant", Content: s.reply, Usage: s.usage}, nil
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

func newTestDeps(t *testing.T, perIPPerHour, maxConcurrent int) Deps {
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
	}
}

func newTestServer(t *testing.T, perIPPerHour, maxConcurrent int) http.Handler {
	t.Helper()
	return NewServer(newTestDeps(t, perIPPerHour, maxConcurrent))
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
	rec := post(t, h, "https://barrahome.org", `{"session_id":"session-0001","message":"hola"}`)

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
	rec := post(t, h, "https://evil.example", `{"session_id":"session-0001","message":"hola"}`)
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
	if rec := post(t, h, "https://barrahome.org", `{"session_id":"session-0001","message":"   "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("blank message status = %d, want 400", rec.Code)
	}
}

// TestStreamRejectsMalformedSessionID pins the charset: anything outside
// [A-Za-z0-9_-]{8,64} is refused, which is what keeps a newline out of the
// server's log lines.
func TestStreamRejectsMalformedSessionID(t *testing.T) {
	cases := map[string]string{
		"newline":       `{"session_id":"good-id-1\nfake log line","message":"x"}`,
		"space":         `{"session_id":"good id 1","message":"x"}`,
		"dot":           `{"session_id":"../../etc/passwd","message":"x"}`,
		"too short":     `{"session_id":"short7c","message":"x"}`,
		"too long":      `{"session_id":"` + strings.Repeat("a", 65) + `","message":"x"}`,
		"unicode":       `{"session_id":"sesión-0001","message":"x"}`,
		"percent":       `{"session_id":"sess%0d%0a01","message":"x"}`,
		"null byte":     `{"session_id":"sess\u0000ion1","message":"x"}`,
		"interior crlf": `{"session_id":"session\r\n0001","message":"x"}`,
	}
	h := newTestServer(t, 20, 10)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := post(t, h, "https://barrahome.org", body); rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, body)
			}
		})
	}
}

func TestStreamRejectsMissingOrigin(t *testing.T) {
	h := newTestServer(t, 20, 10)
	rec := post(t, h, "", `{"session_id":"session-0001","message":"hola"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestStreamCountsMessageInRunes keeps the advertised 4000-character limit
// honest for multi-byte text: 4000 CJK characters are ~12KB and must pass,
// while 4001 must not.
func TestStreamCountsMessageInRunes(t *testing.T) {
	h := newTestServer(t, 20, 10)

	body := `{"session_id":"runes-0001","message":"` + strings.Repeat("字", maxMessageRunes) + `"}`
	if rec := post(t, h, "https://barrahome.org", body); rec.Code != http.StatusOK {
		t.Errorf("4000 runes status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	body = `{"session_id":"runes-0002","message":"` + strings.Repeat("字", maxMessageRunes+1) + `"}`
	if rec := post(t, h, "https://barrahome.org", body); rec.Code != http.StatusBadRequest {
		t.Errorf("4001 runes status = %d, want 400", rec.Code)
	}
}

// TestStreamDistinguishesQuotaFromBusy separates the two rejections that the
// limiter can produce: the hourly quota is 429 (retry later), a saturated
// concurrency cap is 503 (retry now-ish).
func TestStreamDistinguishesQuotaFromBusy(t *testing.T) {
	d := newTestDeps(t, 20, 1)
	model := &blockingModel{started: make(chan struct{}), proceed: make(chan struct{})}
	d.Model = model
	h := NewServer(d)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		post(t, h, "https://barrahome.org", `{"session_id":"holder-0001","message":"one"}`)
	}()

	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the model")
	}

	// A different session, so it is the concurrency cap that trips and not the
	// per-session checkout.
	rec := post(t, h, "https://barrahome.org", `{"session_id":"second-0002","message":"two"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("concurrency-capped status = %d, want 503", rec.Code)
	}

	close(model.proceed)
	wg.Wait()
}

// TestStreamReturns409OnTurnLimit checks a spent session is refused, and that
// the message distinguishes it from the busy-session 409.
func TestStreamReturns409OnTurnLimit(t *testing.T) {
	d := newTestDeps(t, 20, 10)
	d.Sessions = session.NewStore(session.Config{MaxTurns: 1})
	h := NewServer(d)

	if rec := post(t, h, "https://barrahome.org", `{"session_id":"spender-01","message":"one"}`); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec.Code)
	}
	rec := post(t, h, "https://barrahome.org", `{"session_id":"spender-01","message":"two"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second request status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "turn limit") {
		t.Errorf("body = %q, want the turn-limit message", rec.Body.String())
	}
}

// TestStreamReturns503WhenStoreFull covers ErrStoreFull: the store is at its
// cap and the only session in it is checked out, so a new id has nothing to
// evict.
func TestStreamReturns503WhenStoreFull(t *testing.T) {
	d := newTestDeps(t, 20, 10)
	d.Sessions = session.NewStore(session.Config{MaxSessions: 1})
	model := &blockingModel{started: make(chan struct{}), proceed: make(chan struct{})}
	d.Model = model
	h := NewServer(d)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		post(t, h, "https://barrahome.org", `{"session_id":"occupant-1","message":"one"}`)
	}()

	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the model")
	}

	rec := post(t, h, "https://barrahome.org", `{"session_id":"newcomer-1","message":"two"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("store-full status = %d, want 503", rec.Code)
	}

	close(model.proceed)
	wg.Wait()
}

// TestStreamReportsTurnsLeftFromStore pins the minor fix: with Cfg.MaxTurns
// unset the store's own default is the allowance, so turns_left has to come
// from the session rather than a subtraction against the config.
func TestStreamReportsTurnsLeftFromStore(t *testing.T) {
	d := newTestDeps(t, 20, 10)
	d.Cfg.MaxTurns = 0
	d.Sessions = session.NewStore(session.Config{})
	h := NewServer(d)

	rec := post(t, h, "https://barrahome.org", `{"session_id":"counter-01","message":"hola"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	want := fmt.Sprintf(`{"turns_left":%d}`, session.DefaultMaxTurns-1)
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body = %q, want it to contain %s", rec.Body.String(), want)
	}
}

// TestClientIPIgnoresForgedHeaders is the regression for the quota bypass: a
// visitor could pick their own rate-limit bucket with one X-Forwarded-For.
func TestClientIPIgnoresForgedHeaders(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{
			name:       "loopback peer with X-Real-IP",
			remoteAddr: "127.0.0.1:54321",
			headers:    map[string]string{"X-Real-IP": "198.51.100.7"},
			want:       "198.51.100.7",
		},
		{
			name:       "loopback peer with forged X-Forwarded-For only",
			remoteAddr: "127.0.0.1:54321",
			headers:    map[string]string{"X-Forwarded-For": "9.9.9.1, 10.0.0.1"},
			want:       "127.0.0.1",
		},
		{
			name:       "non-loopback peer sending both headers",
			remoteAddr: "203.0.113.9:12345",
			headers: map[string]string{
				"X-Real-IP":       "198.51.100.7",
				"X-Forwarded-For": "9.9.9.1",
			},
			want: "203.0.113.9",
		},
		{
			name:       "IPv6 loopback peer with X-Real-IP",
			remoteAddr: "[::1]:54321",
			headers:    map[string]string{"X-Real-IP": "198.51.100.7"},
			want:       "198.51.100.7",
		},
		{
			name:       "loopback peer with unparseable X-Real-IP",
			remoteAddr: "127.0.0.1:54321",
			headers:    map[string]string{"X-Real-IP": "not-an-ip"},
			want:       "127.0.0.1",
		},
		{
			name:       "RemoteAddr without a port",
			remoteAddr: "203.0.113.9",
			headers:    map[string]string{"X-Forwarded-For": "9.9.9.1"},
			want:       "203.0.113.9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/stream", nil)
			req.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := clientIP(req); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStreamQuotaSurvivesForgedForwardedFor is the end-to-end form of the
// bypass: rotating X-Forwarded-For must not hand a visitor a fresh quota.
func TestStreamQuotaSurvivesForgedForwardedFor(t *testing.T) {
	h := newTestServer(t, 1, 10)
	if rec := post(t, h, "https://barrahome.org", `{"session_id":"forger-001","message":"one"}`); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec.Code)
	}

	for i, forged := range []string{"9.9.9.1", "9.9.9.2", "9.9.9.3"} {
		req := httptest.NewRequest(http.MethodPost, "/stream",
			strings.NewReader(`{"session_id":"forger-001","message":"again"}`))
		req.Header.Set("Origin", "https://barrahome.org")
		req.Header.Set("X-Forwarded-For", forged)
		req.RemoteAddr = "203.0.113.9:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("request %d with X-Forwarded-For %s: status = %d, want 429", i+1, forged, rec.Code)
		}
	}
}

func TestStreamEnforcesPerIPQuota(t *testing.T) {
	h := newTestServer(t, 1, 10)
	if rec := post(t, h, "https://barrahome.org", `{"session_id":"session-0001","message":"one"}`); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec.Code)
	}
	rec := post(t, h, "https://barrahome.org", `{"session_id":"session-0002","message":"two"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request status = %d, want 429", rec.Code)
	}
}

// TestStreamRejectsConcurrentSameSession exercises the Checkout single-flight
// guarantee end-to-end: a second request for a session already mid-stream
// must be refused with 409, not queued behind the first.
func TestStreamRejectsConcurrentSameSession(t *testing.T) {
	d := newTestDeps(t, 20, 10)
	model := &blockingModel{started: make(chan struct{}), proceed: make(chan struct{})}
	d.Model = model
	h := NewServer(d)

	var wg sync.WaitGroup
	var firstRec *httptest.ResponseRecorder
	wg.Add(1)
	go func() {
		defer wg.Done()
		firstRec = post(t, h, "https://barrahome.org", `{"session_id":"busy-session-1","message":"one"}`)
	}()

	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the model")
	}

	rec := post(t, h, "https://barrahome.org", `{"session_id":"busy-session-1","message":"two"}`)
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

// TestHealthzReportsUsageAfterATurn guards the operational-visibility point
// of the whole usage feature: a completed turn's token counts must show up
// on /healthz without parsing logs.
func TestHealthzReportsUsageAfterATurn(t *testing.T) {
	cfg := &config.Config{
		AllowedOrigins: []string{"https://barrahome.org"},
		MaxTurns:       20,
		MaxTokens:      256,
		MaxToolRounds:  4,
		SessionTTL:     30 * time.Minute,
	}
	deps := Deps{
		Cfg:   cfg,
		Tools: stubTools{},
		Model: &stubModel{reply: "hola", usage: &moonshot.Usage{
			PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedTokens: 40,
		}},
		Sessions: session.NewStore(session.Config{TTL: cfg.SessionTTL, MaxTurns: cfg.MaxTurns}),
		Limiter:  limits.NewLimiter(20, 10),
	}
	h := NewServer(deps)
	post(t, h, "https://barrahome.org", `{"session_id":"session-0001","message":"hola"}`)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"prompt_tokens":100`, `"completion_tokens":10`, `"cached_tokens":40`, `"turns":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("healthz body = %s, want it to contain %s", body, want)
		}
	}
}
