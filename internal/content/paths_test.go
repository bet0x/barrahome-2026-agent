package content

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "2026", "07"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "2026", "07", "post.md"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	return r, root
}

func TestResolveAcceptsPathsInsideRoot(t *testing.T) {
	r, root := newTestResolver(t)

	for _, rel := range []string{"", ".", "2026", "2026/07/post.md", "./2026/07/post.md"} {
		got, err := r.Resolve(rel)
		if err != nil {
			t.Errorf("Resolve(%q) returned error: %v", rel, err)
			continue
		}
		if !strings.HasPrefix(got, root) {
			t.Errorf("Resolve(%q) = %q, want a path under %q", rel, got, root)
		}
	}
}

func TestResolveRejectsEscapes(t *testing.T) {
	r, _ := newTestResolver(t)

	bad := []string{
		"..",
		"../",
		"../etc/passwd",
		"2026/../../etc/passwd",
		"/etc/passwd",
		"/",
		"2026/07/../../../etc/shadow",
		"foo/../../bar",
		"\x00abc",
	}
	for _, rel := range bad {
		if got, err := r.Resolve(rel); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Resolve(%q) = (%q, %v), want ErrInvalidPath", rel, got, err)
		}
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	r, root := newTestResolver(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, err := r.Resolve("escape"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Resolve(\"escape\") = (%q, %v), want ErrInvalidPath for a symlink leaving the root", got, err)
	}
}

// TestResolveRejectsSymlinkedParentEscape covers a symlinked *parent*
// component rather than the leaf itself: root/link -> outside, where outside
// is a real directory outside the root. Resolve must reject both a leaf that
// does not exist yet under the symlinked parent and a leaf that does exist,
// since in both cases the OS would actually open the path under outside, not
// under root.
func TestResolveRejectsSymlinkedParentEscape(t *testing.T) {
	r, root := newTestResolver(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "existing.txt"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, rel := range []string{"link/missing.txt", "link/existing.txt"} {
		if got, err := r.Resolve(rel); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Resolve(%q) = (%q, %v), want ErrInvalidPath for a symlinked parent leaving the root", rel, got, err)
		}
	}
}
