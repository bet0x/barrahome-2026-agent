package main

import (
	"testing"

	"github.com/bet0x/barrahome-2026-agent/internal/config"
)

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
