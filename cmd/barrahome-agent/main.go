// Command barrahome-agent serves the terminal agent for barrahome.org.
//
// It runs in two modes. "supervise" is the container entrypoint: it builds the
// sandlock policy and runs "serve" as a confined child, because sandlock's
// network policy cannot be applied to the current process (Confine honors
// filesystem rules only). "serve" is the HTTP server itself.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	sandlock "github.com/multikernel/sandlock/go"

	"github.com/bet0x/barrahome-2026-agent/internal/config"
	"github.com/bet0x/barrahome-2026-agent/internal/content"
	"github.com/bet0x/barrahome-2026-agent/internal/httpapi"
	"github.com/bet0x/barrahome-2026-agent/internal/limits"
	"github.com/bet0x/barrahome-2026-agent/internal/moonshot"
	"github.com/bet0x/barrahome-2026-agent/internal/sandbox"
	"github.com/bet0x/barrahome-2026-agent/internal/session"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "supervise":
		if err := supervise(); err != nil {
			log.Fatalf("supervise: %v", err)
		}
	case "serve":
		if err := serve(); err != nil {
			log.Fatalf("serve: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "usage: %s [supervise|serve]\n", os.Args[0])
		os.Exit(2)
	}
}

// supervise applies the sandbox policy and runs the worker under it. If the
// policy cannot be applied it returns an error: the worker must never run
// unconfined.
func supervise() error {
	// A stale kernel produces an opaque "failed to create sandbox" error from
	// deep inside sandlock; check the ABI up front so the failure is legible.
	if v, min := sandlock.LandlockABIVersion(), sandlock.MinLandlockABI(); v < min {
		return fmt.Errorf("kernel Landlock ABI v%d is below the required v%d", v, min)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating own binary: %w", err)
	}

	sb := sandbox.Policy(workerPolicy(cfg))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("supervising confined worker (workspace=%s port=%d)", cfg.Workspace, cfg.ListenPort)
	code, err := sandbox.RunWorker(ctx, sb, self, "serve")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("worker exited with code %d", code)
	}
	return nil
}

// workerPolicy describes the worker's confinement.
//
// MaxMemory is deliberately unset: sandlock enforces it by summing anonymous
// mmap lengths, and the Go runtime's up-front arena reservation exceeds any
// sane limit, so the worker is killed before it runs (see sandbox.Options).
// The container's cgroup limit is what caps the worker's memory.
func workerPolicy(cfg *config.Config) sandbox.Options {
	return sandbox.Options{
		Workspace:    cfg.Workspace,
		ListenPort:   cfg.ListenPort,
		MaxProcesses: 16,
		MaxOpenFiles: 256,
		MaxCPU:       50,
	}
}

// serve runs the HTTP server. It is the process that is actually confined.
func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	resolver, err := content.NewResolver(cfg.Workspace)
	if err != nil {
		return err
	}

	sessions := session.NewStore(session.Config{TTL: cfg.SessionTTL, MaxTurns: cfg.MaxTurns})
	limiter := limits.NewLimiter(cfg.PerIPPerHour, cfg.MaxConcurrent)

	stop := make(chan struct{})
	defer close(stop)
	sessions.StartSweeper(5*time.Minute, stop)
	limiter.StartSweeper(5*time.Minute, stop)

	handler := httpapi.NewServer(httpapi.Deps{
		Cfg:      cfg,
		Tools:    content.NewTools(resolver),
		Model:    moonshot.NewClient(cfg.MoonshotBaseURL, cfg.MoonshotAPIKey, cfg.Model, &http.Client{Timeout: 120 * time.Second}),
		Sessions: sessions,
		Limiter:  limiter,
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", cfg.ListenPort),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE responses are long-lived by design.
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("serving on %s (model=%s)", srv.Addr, cfg.Model)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
