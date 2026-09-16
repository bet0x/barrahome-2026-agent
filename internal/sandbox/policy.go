// Package sandbox builds the sandlock policy that confines the agent worker.
package sandbox

import (
	"context"
	"fmt"
	"syscall"
	"time"

	sandlock "github.com/multikernel/sandlock/go"
)

// MoonshotHostPort is the single egress destination the worker may reach.
const MoonshotHostPort = "api.moonshot.ai:443"

// Options describes the confinement the worker runs under.
type Options struct {
	Workspace  string // absolute path to the read-only content directory
	ListenPort int
	// MaxMemory is a sandlock byte-size ("64M"); "" leaves it unset. The
	// worker leaves it unset and lets the cgroup cap memory instead, because
	// a cgroup counts resident set size while sandlock counts mapped length,
	// and RSS is the thing we mean.
	//
	// Until sandlock v0.8.8 that was not a preference. The accounting summed
	// the length of every anonymous mmap, including the large PROT_NONE arena
	// the Go runtime reserves at startup, so any limit small enough to be
	// useful killed the worker before main ran. v0.8.8 stopped charging
	// PROT_NONE reservations and a Go child now survives a real limit. This
	// checkout is pinned below that (third_party/sandlock), so here the old
	// behaviour still applies and the field must stay empty for the worker.
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
		NetAllow: []string{MoonshotHostPort},
		// The container holds CAP_SYS_PTRACE so the supervisor can call
		// pidfd_getfd under Docker's default seccomp profile (see
		// compose.yaml), and the worker inherits the capability. sandlock's
		// default blocklist already denies the child ptrace and
		// process_vm_readv/writev; pidfd_getfd is the one syscall that
		// capability unlocks that it does not cover, and the worker has no
		// use for it.
		ExtraDenySyscalls: []string{"pidfd_getfd"},
		NetAllowBind:      []string{fmt.Sprintf("%d", opts.ListenPort)},
		MaxMemory:         opts.MaxMemory,
		MaxProcesses:      opts.MaxProcesses,
		MaxOpenFiles:      opts.MaxOpenFiles,
		MaxCPU:            opts.MaxCPU,
		Name:              "barrahome-agent",
	}
}

// RunWorker runs argv under the sandbox with the caller's stdio attached and
// returns the child's exit code. An error means the policy could not be
// applied; the caller must not fall back to running unconfined.
//
// When ctx is cancelled the worker is sent SIGTERM so it can drain, and
// SIGKILL if it is still alive killGrace later. killGrace must outlast the
// worker's own shutdown deadline, or the supervisor kills it mid-drain — the
// caller derives it from that deadline rather than picking an independent
// constant that can drift out of sync. RunInteractive is not used: it only
// checks ctx on entry, so the worker would never learn that the supervisor
// was asked to stop.
func RunWorker(ctx context.Context, sb *sandlock.Sandbox, killGrace time.Duration, argv ...string) (int, error) {
	if len(argv) == 0 {
		return 0, fmt.Errorf("sandbox: empty argv")
	}
	// The zero Stdio inherits all three streams, so the worker's logs land on
	// the supervisor's own stdout and stderr.
	proc, err := sb.Popen(sandlock.Stdio{}, argv...)
	if err != nil {
		return 0, fmt.Errorf("sandbox: applying policy: %w", err)
	}

	exited := make(chan struct{})
	defer close(exited)
	go func() {
		select {
		case <-exited:
			return
		case <-ctx.Done():
		}
		// The sandboxed child leads its own process group.
		_ = syscall.Kill(-proc.Pid(), syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(killGrace):
			_ = proc.Kill()
		}
	}()

	res, err := proc.Wait()
	if err != nil {
		return 0, fmt.Errorf("sandbox: waiting for worker: %w", err)
	}
	return res.ExitCode, nil
}
