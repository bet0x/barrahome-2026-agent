// Package content implements the read-only tools the model may call over the
// curated content directory.
package content

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidPath is returned for any path that is absolute, contains a NUL,
// contains a ".." segment, or resolves outside the content root.
var ErrInvalidPath = errors.New("invalid path")

// Resolver turns model-supplied relative paths into absolute paths that are
// guaranteed to sit inside the content root. Landlock is the real
// containment boundary; this is defense in depth and stays strict regardless.
type Resolver struct {
	root string
}

// NewResolver resolves root to an absolute, symlink-free path.
func NewResolver(root string) (*Resolver, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("content: resolving root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("content: resolving root: %w", err)
	}
	return &Resolver{root: real}, nil
}

// Root returns the absolute content root.
func (r *Resolver) Root() string { return r.root }

// Resolve validates rel and returns the absolute path it denotes.
//
// ".." segments are rejected explicitly rather than left to filepath.Clean,
// which would silently collapse a leading ".." (Clean("/..") == "/") and let
// an escaping input look like it resolved to the root. The prefix check
// below is still needed afterward to catch symlinks inside the root that
// point outside of it.
func (r *Resolver) Resolve(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: contains NUL", ErrInvalidPath)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: must be relative", ErrInvalidPath)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: contains \"..\" segment", ErrInvalidPath)
		}
	}

	joined := filepath.Join(r.root, rel)

	real, err := resolveExistingAncestor(joined)
	if err != nil {
		// Classify rather than pass the OS error through: it is an
		// *fs.PathError over the resolved absolute path, and that must
		// never reach the model or a visitor.
		return "", fmt.Errorf("%w: %s", ErrInvalidPath, describeOSError(err))
	}
	joined = real

	if joined != r.root && !strings.HasPrefix(joined, r.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: escapes the content root", ErrInvalidPath)
	}
	return joined, nil
}

// resolveExistingAncestor resolves symlinks along path. If path (or a chain
// of its trailing components) does not exist yet, it walks up to the nearest
// existing ancestor, resolves symlinks there, and re-appends the literal
// trailing components — a missing leaf must not let a symlinked parent
// directory escape detection.
func resolveExistingAncestor(path string) (string, error) {
	var trailing []string
	cur := path
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(trailing) - 1; i >= 0; i-- {
				real = filepath.Join(real, trailing[i])
			}
			return real, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Filesystem root reached with nothing resolvable; give up.
			return "", err
		}
		trailing = append(trailing, filepath.Base(cur))
		cur = parent
	}
}
