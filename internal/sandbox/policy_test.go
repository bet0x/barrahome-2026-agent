package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testOptions mirrors the worker's own policy, which leaves MaxMemory unset;
// the tests that care about it set it themselves.
func testOptions(t *testing.T, workspace string) Options {
	t.Helper()
	return Options{
		Workspace:    workspace,
		ListenPort:   9000,
		MaxProcesses: 16,
		MaxOpenFiles: 256,
		MaxCPU:       50,
	}
}

// TestPolicyConfinesFilesystem is the core guarantee: the worker can read the
// workspace and nothing else on the host.
func TestPolicyConfinesFilesystem(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "post.md"), []byte("hello workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := Policy(testOptions(t, ws))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := sb.Run(ctx, "cat", filepath.Join(ws, "post.md"))
	if err != nil {
		t.Fatalf("running allowed read: %v", err)
	}
	if got := string(res.Stdout); !strings.Contains(got, "hello workspace") {
		t.Errorf("workspace file should be readable, got exit=%d stdout=%q stderr=%q",
			res.ExitCode, got, res.Stderr)
	}

	res, err = sb.Run(ctx, "cat", outside)
	if err != nil {
		t.Fatalf("running denied read: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("file outside workspace must not be readable, but cat succeeded: %q", res.Stdout)
	}

	res, err = sb.Run(ctx, "cat", "/etc/shadow")
	if err != nil {
		t.Fatalf("running /etc/shadow read: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("/etc/shadow must not be readable; policy is granting /etc too broadly")
	}
}

// TestPolicyRestrictsEgress asserts the only reachable destination is Moonshot.
func TestPolicyRestrictsEgress(t *testing.T) {
	sb := Policy(testOptions(t, t.TempDir()))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Allowed: reaching Moonshot without a key yields HTTP 401, which proves
	// the connection and TLS handshake completed.
	res, err := sb.Run(ctx, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"--max-time", "25", "https://api.moonshot.ai/v1/models")
	if err != nil {
		t.Fatalf("running allowed egress: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "401" {
		t.Errorf("api.moonshot.ai should be reachable (expect HTTP 401), got %q stderr=%q", got, res.Stderr)
	}

	for _, target := range []string{"https://example.com", "https://1.1.1.1"} {
		res, err := sb.Run(ctx, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
			"--max-time", "10", target)
		if err != nil {
			t.Fatalf("running denied egress %s: %v", target, err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != "000" {
			t.Errorf("%s must be unreachable (expect http_code 000), got %q", target, got)
		}
	}
}

// TestPolicyStartsGoChild is the regression for a policy that the worker
// could never run under: sandlock enforces MaxMemory by summing anonymous
// mmap lengths, and the Go runtime reserves its heap arenas up front, so any
// MaxMemory kills a Go child before main. The test binary is itself a Go
// program, so re-running one trivial test under the policy is the cheapest
// honest check that the worker can start.
func TestPolicyStartsGoChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// The workspace grant is what makes the test binary readable/executable.
	opts := testOptions(t, filepath.Dir(exe))
	opts.MaxMemory = ""
	sb := Policy(opts)
	// This binary loads libsandlock_ffi through an rpath into the checkout,
	// where the deployed worker loads it from a directory the policy already
	// covers.
	sb.FSReadable = append(sb.FSReadable, ffiLibDir(t))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := sb.Run(ctx, exe, "-test.run=^TestGoChildStarts$", "-test.count=1")
	if err != nil {
		t.Fatalf("running the Go child: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("a Go child must start under the worker policy, got exit=%d stdout=%q stderr=%q",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestGoChildStarts is the body TestPolicyStartsGoChild re-execs under the
// sandbox. Reaching it is the whole assertion.
func TestGoChildStarts(t *testing.T) {}

// ffiLibDir is the checkout's cargo output directory, which the sandlock_repo
// build tag rpaths into every binary in this module.
func ffiLibDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating this test's source file")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "third_party", "sandlock", "target", "release")
}

// TestPolicyEnforcesMemoryLimit guards the resource cap that protects the
// shared VPS from a runaway worker.
func TestPolicyEnforcesMemoryLimit(t *testing.T) {
	opts := testOptions(t, t.TempDir())
	opts.MaxMemory = "64M"
	sb := Policy(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := sb.Run(ctx, "python3", "-c", "b=bytearray(256*1024*1024);print('allocated')")
	if err != nil {
		t.Fatalf("running allocation: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("256MB allocation should fail under MaxMemory=64M, got exit=0 stdout=%q", res.Stdout)
	}
}

// TestPolicyEnforcesBindAllowlist guards NetAllowBind, which sandlock silently
// stopped enforcing whenever NetAllow was also set (our exact combination)
// until the fix in v0.8.8. The worker binds one port and nothing else in the
// process listens, so the practical exposure was nil, but a control that stops
// enforcing without saying so is worth a test rather than trust.
func TestPolicyEnforcesBindAllowlist(t *testing.T) {
	opts := testOptions(t, t.TempDir())
	// Not the worker's real port: the dev container publishes that on the
	// host, and EADDRINUSE would be indistinguishable from a refused bind.
	opts.ListenPort = 18080
	sb := Policy(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bind := "import socket;s=socket.socket();s.bind(('127.0.0.1',%d));print('bound')"

	res, err := sb.Run(ctx, "python3", "-c", fmt.Sprintf(bind, opts.ListenPort))
	if err != nil {
		t.Fatalf("running allowed bind: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("the listen port must be bindable, got exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	res, err = sb.Run(ctx, "python3", "-c", fmt.Sprintf(bind, 18099))
	if err != nil {
		t.Fatalf("running denied bind: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("a port outside NetAllowBind must not be bindable, but bind succeeded: %q", res.Stdout)
	}
}
