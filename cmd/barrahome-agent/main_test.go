package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bet0x/barrahome-2026-agent/internal/config"
)

// TestRunRequiresSubcommand pins the entrypoint guard: "serve" is the
// unconfined worker, so an empty or unknown argv must exit 2 with usage
// instead of starting it. Exit code 2 also distinguishes this from a failed
// serve, which returns 1.
func TestRunRequiresSubcommand(t *testing.T) {
	for _, argv := range [][]string{
		{"barrahome-agent"},
		{"barrahome-agent", "srve"},
		{"barrahome-agent", ""},
	} {
		var stderr strings.Builder
		if code := run(argv, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2", argv, code)
		}
		if !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("run(%q) stderr = %q, want usage", argv, stderr.String())
		}
	}
}

// TestWorkerPolicyHasNoMemoryLimit guards Critical 2: sandlock's MaxMemory is
// enforced over anonymous mmap length, which kills a Go child during its
// arena reservation. The cgroup caps the worker's memory instead.
func TestWorkerPolicyHasNoMemoryLimit(t *testing.T) {
	opts := workerPolicy(&config.Config{Workspace: "/workspace", ListenPort: 9000})
	if opts.MaxMemory != "" {
		t.Errorf("MaxMemory = %q, want it unset", opts.MaxMemory)
	}
	if opts.MaxProcesses == 0 || opts.MaxOpenFiles == 0 || opts.MaxCPU == 0 {
		t.Errorf("the limits that do work must stay set: %+v", opts)
	}
}

// TestWorkerKillGraceOutlastsShutdownTimeout pins the ordering that keeps the
// supervisor's SIGKILL from racing the worker's own drain: whatever the
// configured drain deadline is, the derived kill grace must stay strictly
// longer, with no independent constant to fall out of sync.
func TestWorkerKillGraceOutlastsShutdownTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second} {
		cfg := &config.Config{ShutdownTimeout: timeout}
		if got := workerKillGrace(cfg); got <= timeout {
			t.Errorf("workerKillGrace(%v) = %v, want strictly more than the drain deadline", timeout, got)
		}
	}
}
