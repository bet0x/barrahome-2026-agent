package content

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
