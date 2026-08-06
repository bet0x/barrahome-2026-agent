// Package httpapi exposes the agent over HTTP with a server-sent-event stream.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bet0x/barrahome-2026-agent/internal/agentloop"
	"github.com/bet0x/barrahome-2026-agent/internal/config"
	"github.com/bet0x/barrahome-2026-agent/internal/limits"
	"github.com/bet0x/barrahome-2026-agent/internal/session"
)

// Deps are the server's collaborators.
type Deps struct {
	Cfg      *config.Config
	Tools    agentloop.ToolRunner
	Model    agentloop.Streamer
	Sessions *session.Store
	Limiter  *limits.Limiter
}

type streamRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// NewServer wires the routes. nginx terminates the public side and strips the
// /ai-proxy/ prefix, so paths here are unprefixed.
func NewServer(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/stream", d.handleStream)
	return mux
}

func (d Deps) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !d.originAllowed(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	var req streamRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Message = strings.TrimSpace(req.Message)
	if req.SessionID == "" || len(req.SessionID) > 128 {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	if req.Message == "" || len(req.Message) > 4000 {
		http.Error(w, "message must be between 1 and 4000 characters", http.StatusBadRequest)
		return
	}

	now := time.Now()
	release, err := d.Limiter.Acquire(clientIP(r), now)
	if err != nil {
		switch {
		case errors.Is(err, limits.ErrPerIPQuota):
			http.Error(w, "rate limit reached, try again later", http.StatusTooManyRequests)
		default:
			http.Error(w, "busy, try again shortly", http.StatusServiceUnavailable)
		}
		return
	}
	defer release()

	// Checkout reserves the turn and exclusive access atomically. The release
	// is non-nil on every path, including the error ones, so it is deferred
	// immediately; sess must not be touched once it runs.
	sess, releaseSession, err := d.Sessions.Checkout(req.SessionID, now)
	defer releaseSession()
	if err != nil {
		switch {
		case errors.Is(err, session.ErrSessionBusy):
			http.Error(w, "a request for this session is already running", http.StatusConflict)
		case errors.Is(err, session.ErrTurnLimit):
			http.Error(w, "session turn limit reached, start a new one", http.StatusConflict)
		case errors.Is(err, session.ErrStoreFull):
			http.Error(w, "server busy, try again shortly", http.StatusServiceUnavailable)
		default:
			http.Error(w, "session unavailable", http.StatusInternalServerError)
		}
		return
	}

	// Past this point the response is a stream, so failures are SSE events
	// rather than status codes.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	send := func(event string, payload any) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	history, runErr := agentloop.Run(r.Context(), agentloop.Deps{
		Model:         d.Model,
		Tools:         d.Tools,
		MaxTokens:     d.Cfg.MaxTokens,
		MaxToolRounds: d.Cfg.MaxToolRounds,
	}, sess.Messages, req.Message, func(e agentloop.Event) error {
		return send(string(e.Kind), map[string]string{"text": e.Text})
	})

	if runErr != nil {
		log.Printf("agent run failed (session %s): %v", req.SessionID, runErr)
		_ = send("error", map[string]string{"message": "the agent hit an error, please try again"})
		return
	}

	// The turn was already spent by Checkout; only the history is ours to
	// write, and only until releaseSession runs.
	sess.Messages = history
	turnsLeft := d.Cfg.MaxTurns - sess.Turns

	_ = send("done", map[string]any{"turns_left": turnsLeft})
}

func (d Deps) originAllowed(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false // the browser always sends Origin on cross-origin POSTs
	}
	for _, allowed := range d.Cfg.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

// clientIP prefers the left-most X-Forwarded-For entry, since nginx (behind
// Caddy) is what actually talks to this process.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
