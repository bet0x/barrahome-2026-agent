package content

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newTestTools(t *testing.T) (*Tools, string) {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("cv.md", "Alberto Ferrer, SRE\n")
	mustWrite("2026/07/nginx.md", "# nginx markdown\nmarkdown_filter on;\n")
	mustWrite("2026/07/vllm.md", "# vllm\nKV offloading notes\n")

	r, err := NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	return NewTools(r), root
}

func TestListDir(t *testing.T) {
	tools, _ := newTestTools(t)

	out, err := tools.ListDir("")
	if err != nil {
		t.Fatalf("ListDir(\"\"): %v", err)
	}
	for _, want := range []string{"cv.md", "2026/"} {
		if !strings.Contains(out, want) {
			t.Errorf("ListDir(\"\") = %q, want it to mention %q", out, want)
		}
	}

	out, err = tools.ListDir("2026/07")
	if err != nil {
		t.Fatalf("ListDir(\"2026/07\"): %v", err)
	}
	if !strings.Contains(out, "nginx.md") || !strings.Contains(out, "vllm.md") {
		t.Errorf("ListDir(\"2026/07\") = %q, want both posts", out)
	}
}

func TestReadFile(t *testing.T) {
	tools, _ := newTestTools(t)

	out, err := tools.ReadFile("2026/07/nginx.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(out, "markdown_filter on;") {
		t.Errorf("ReadFile = %q, want the file body", out)
	}

	if _, err := tools.ReadFile("../../etc/passwd"); err == nil {
		t.Error("ReadFile(\"../../etc/passwd\") should fail")
	}
	if _, err := tools.ReadFile("2026"); err == nil {
		t.Error("ReadFile on a directory should fail")
	}
}

func TestReadFileEmptyFile(t *testing.T) {
	tools, root := newTestTools(t)
	if err := os.WriteFile(filepath.Join(root, "empty.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := tools.ReadFile("empty.md")
	if err != nil {
		t.Fatalf("ReadFile(empty.md): %v", err)
	}
	if out == "" {
		t.Error("ReadFile on an empty file returned \"\", want an explicit note")
	}
	if !strings.Contains(strings.ToLower(out), "empty") {
		t.Errorf("ReadFile(empty.md) = %q, want it to say the file is empty", out)
	}
}

func TestReadFileTruncatesLargeFiles(t *testing.T) {
	tools, root := newTestTools(t)
	big := strings.Repeat("x", MaxFileBytes+5000)
	if err := os.WriteFile(filepath.Join(root, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := tools.ReadFile("big.md")
	if err != nil {
		t.Fatalf("ReadFile(big): %v", err)
	}
	if len(out) > MaxFileBytes+200 {
		t.Errorf("ReadFile returned %d bytes, want it capped near %d", len(out), MaxFileBytes)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("truncated output should say so, got tail %q", out[max(0, len(out)-80):])
	}
}

func TestReadFileErrorHidesAbsolutePath(t *testing.T) {
	tools, root := newTestTools(t)

	_, err := tools.ReadFile("missing.md")
	if err == nil {
		t.Fatal("ReadFile(missing.md) should fail")
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("ReadFile error leaked the absolute content root: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Errorf("ReadFile error = %v, want it to say the file was not found", err)
	}
}

// TestReadFileTruncationSurvivesConcurrentGrowth reproduces the grow-after-stat
// race: a file grows past MaxFileBytes between the Stat and the Read inside
// ReadFile. The truncation notice must depend only on what was actually read,
// never on the pre-read Stat, so the invariant below must hold on every call
// regardless of how far the concurrent writer has gotten.
func TestReadFileTruncationSurvivesConcurrentGrowth(t *testing.T) {
	tools, root := newTestTools(t)
	path := filepath.Join(root, "growing.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("z", 10)), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		chunk := []byte(strings.Repeat("z", 4096))
		for {
			select {
			case <-stop:
				return
			default:
				f.Write(chunk)
			}
		}
	}()

	for i := 0; i < 200; i++ {
		out, err := tools.ReadFile("growing.md")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		// A body landing exactly on the cap with no notice is the signature
		// of the old bug: the file already had more bytes at read time, but
		// a stat taken before the growth said otherwise.
		if len(out) == MaxFileBytes && !strings.Contains(out, "truncated") {
			t.Fatalf("iteration %d: returned exactly %d bytes with no truncation notice", i, MaxFileBytes)
		}
	}
	close(stop)
	<-done
}

func TestReadFileRejectsFIFO(t *testing.T) {
	tools, root := newTestTools(t)
	fifoPath := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("cannot create a FIFO on this platform: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := tools.ReadFile("pipe")
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Error("ReadFile(pipe) should fail instead of returning FIFO contents")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadFile blocked on a FIFO instead of rejecting it up front")
	}
}

func TestSearchContentSkipsDotfiles(t *testing.T) {
	tools, root := newTestTools(t)
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET_TOKEN=abc123"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := tools.SearchContent("SECRET_TOKEN")
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if strings.Contains(out, ".env") {
		t.Errorf("SearchContent leaked a dotfile: %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "no match") {
		t.Errorf("SearchContent = %q, want no matches reported", out)
	}
}

func TestSearchContent(t *testing.T) {
	tools, _ := newTestTools(t)

	out, err := tools.SearchContent("KV offloading")
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if !strings.Contains(out, "2026/07/vllm.md") {
		t.Errorf("SearchContent = %q, want the matching file path", out)
	}

	out, err = tools.SearchContent("zzz-not-present-zzz")
	if err != nil {
		t.Fatalf("SearchContent(miss): %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "no match") {
		t.Errorf("SearchContent miss = %q, want it to report no matches", out)
	}

	if _, err := tools.SearchContent(""); err == nil {
		t.Error("SearchContent(\"\") should fail")
	}
}

func TestCallDispatchReturnsTextOnError(t *testing.T) {
	tools, _ := newTestTools(t)

	if got := tools.Call("read_file", `{"path":"cv.md"}`); !strings.Contains(got, "Alberto Ferrer") {
		t.Errorf("Call(read_file) = %q, want file contents", got)
	}
	if got := tools.Call("read_file", `{"path":"../etc/passwd"}`); !strings.Contains(strings.ToLower(got), "error") {
		t.Errorf("Call with bad path = %q, want an error string for the model", got)
	}
	if got := tools.Call("no_such_tool", `{}`); !strings.Contains(strings.ToLower(got), "unknown tool") {
		t.Errorf("Call(no_such_tool) = %q, want an unknown-tool message", got)
	}
	if got := tools.Call("read_file", `not json`); !strings.Contains(strings.ToLower(got), "error") {
		t.Errorf("Call with bad JSON = %q, want an error string", got)
	}
}
