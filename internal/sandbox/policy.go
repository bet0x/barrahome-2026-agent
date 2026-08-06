// Package sandbox builds the sandlock policy that confines the agent worker.
package sandbox

import (
	"context"
	"fmt"

	sandlock "github.com/multikernel/sandlock/go"
)

// MoonshotHostPort is the single egress destination the worker may reach.
const MoonshotHostPort = "api.moonshot.ai:443"

// Options describes the confinement the worker runs under.
type Options struct {
	Workspace  string // absolute path to the read-only content directory
	ListenPort int
	// MaxMemory is a sandlock byte-size ("64M"); "" leaves it unset. It is
	// enforced by summing the length of every anonymous mmap, which a Go
	// child cannot survive: the runtime reserves its heap arenas up front, so
	// any limit smaller than that reservation kills the process at startup.
	// Leave it unset for Go children and cap their memory with a cgroup.
	MaxMemory    string
	MaxProcesses uint32
	MaxOpenFiles uint32
	MaxCPU       uint8 // percent of one core, 1-100; 0 = unset
}

// Policy builds the sandlock sandbox for the worker.
//
// FSReadable covers only what the worker needs (workspace, cgo shared libs,
// TLS roots, hosts/resolv.conf) and lists /etc's files individually rather
// than granting /etc wholesale, which would expose /etc/shadow. There is no
// FSWritable grant: sandlock bundles full read rights into any write grant,
// so a blanket FSWritable("/tmp") would expose other processes' temp files.
// NetAllowBind documents intent but isn't a control we rely on — sandlock
// silently drops the bind allowlist whenever NetAllow is also set.
func Policy(opts Options) *sandlock.Sandbox {
	return &sandlock.Sandbox{
		FSReadable: []string{
			opts.Workspace,
			"/usr", "/lib", "/lib64", "/bin",
			"/etc/ssl/certs",
			"/etc/hosts",
			"/etc/resolv.conf",
		},
		NetAllow:     []string{MoonshotHostPort},
		NetAllowBind: []string{fmt.Sprintf("%d", opts.ListenPort)},
		MaxMemory:    opts.MaxMemory,
		MaxProcesses: opts.MaxProcesses,
		MaxOpenFiles: opts.MaxOpenFiles,
		MaxCPU:       opts.MaxCPU,
		Name:         "barrahome-agent",
	}
}

// RunWorker runs argv under the sandbox with the caller's stdio attached and
// returns the child's exit code. An error means the policy could not be
// applied; the caller must not fall back to running unconfined.
func RunWorker(ctx context.Context, sb *sandlock.Sandbox, argv ...string) (int, error) {
	if len(argv) == 0 {
		return 0, fmt.Errorf("sandbox: empty argv")
	}
	code, err := sb.RunInteractive(ctx, argv...)
	if err != nil {
		return 0, fmt.Errorf("sandbox: applying policy: %w", err)
	}
	return code, nil
}
