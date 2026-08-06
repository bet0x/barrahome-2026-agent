package main

import (
	"strings"
	"testing"

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
