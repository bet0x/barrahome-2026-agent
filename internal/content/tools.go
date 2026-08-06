package content

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// MaxFileBytes caps a single read_file result.
	MaxFileBytes = 64 * 1024
	// MaxSearchMatches caps how many hits search_content reports.
	MaxSearchMatches = 40
	// maxSearchFileBytes skips files too large to be blog content.
	maxSearchFileBytes = 1 << 20
)

// Tools implements the read-only operations exposed to the model. Every
// operation is a native Go file call — nothing here shells out, writes, or
// touches the network, which is what keeps prompt injection from having an
// exfiltration path.
type Tools struct {
	r *Resolver
}

// NewTools returns tools rooted at r.
func NewTools(r *Resolver) *Tools { return &Tools{r: r} }

// describeOSError classifies a filesystem error without repeating its text,
// which for *fs.PathError embeds the resolved absolute host path.
func describeOSError(err error) string {
	switch {
	case os.IsNotExist(err):
		return "file not found"
	case os.IsPermission(err):
		return "permission denied"
	default:
		return "unavailable"
	}
}

// ListDir lists the entries of a directory relative to the content root.
// Directories are suffixed with "/" so the model can navigate without guessing.
func (t *Tools) ListDir(rel string) (string, error) {
	abs, err := t.r.Resolve(rel)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("cannot list %q: %s", rel, describeOSError(err))
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() {
			names = append(names, e.Name()+"/")
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Sprintf("%s is empty", displayPath(rel)), nil
	}
	return fmt.Sprintf("%s:\n%s", displayPath(rel), strings.Join(names, "\n")), nil
}

// ReadFile returns the contents of a file, truncated to MaxFileBytes.
func (t *Tools) ReadFile(rel string) (string, error) {
	abs, err := t.r.Resolve(rel)
	if err != nil {
		return "", err
	}
	// Stat only to reject directories and non-regular files (FIFOs, sockets,
	// devices) before opening — opening a FIFO with no writer blocks forever.
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read %q: %s", rel, describeOSError(err))
	}
	if info.IsDir() {
		return "", fmt.Errorf("%q is a directory, not a file", rel)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", rel)
	}

	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read %q: %s", rel, describeOSError(err))
	}
	defer f.Close()

	// Read one byte past the cap and decide truncation from what was
	// actually read, not from the stat above: the file can grow between
	// the two calls, and a size caught before that growth would otherwise
	// describe a truncated read as complete.
	buf, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("cannot read %q: %s", rel, describeOSError(err))
	}
	if len(buf) > MaxFileBytes {
		return string(buf[:MaxFileBytes]) + fmt.Sprintf("\n\n[truncated: showing the first %d bytes]", MaxFileBytes), nil
	}
	return string(buf), nil
}

// SearchContent does a case-insensitive substring search across the content
// tree, reporting matching file paths with the matching line.
func (t *Tools) SearchContent(query string) (string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("query must not be empty")
	}
	needle := strings.ToLower(q)

	var matches []string
	err := filepath.WalkDir(t.r.Root(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting the walk
		}
		if len(matches) >= MaxSearchMatches {
			return fs.SkipAll
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != t.r.Root() {
				return fs.SkipDir
			}
			return nil
		}
		// Match ListDir's visibility model: dotfiles (.env and friends)
		// stay hidden from search results too.
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxSearchFileBytes {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(t.r.Root(), path)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(strings.ToLower(line), needle) {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if len(trimmed) > 200 {
				trimmed = trimmed[:200] + "…"
			}
			matches = append(matches, fmt.Sprintf("%s:%d: %s", rel, i+1, trimmed))
			break // one hit per file keeps results broad rather than deep
		}
		return nil
	})
	if err != nil {
		// Defensive: the callback above only ever returns nil, fs.SkipDir or
		// fs.SkipAll, so WalkDir cannot actually hand back a non-nil error
		// here. Kept as a guard in case that callback ever changes.
		return "", fmt.Errorf("search failed: %w", err)
	}
	if len(matches) == 0 {
		return fmt.Sprintf("no matches for %q", q), nil
	}
	sort.Strings(matches)
	return strings.Join(matches, "\n"), nil
}

// Call dispatches a tool call by name. It never returns an error: tool
// failures are returned as text so the model can react conversationally
// instead of the stream aborting.
func (t *Tools) Call(name, argumentsJSON string) string {
	var args struct {
		Path  string `json:"path"`
		Query string `json:"query"`
	}
	if strings.TrimSpace(argumentsJSON) == "" {
		argumentsJSON = "{}"
	}
	if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil {
		return fmt.Sprintf("error: could not parse arguments: %v", err)
	}

	var (
		out string
		err error
	)
	switch name {
	case "list_dir":
		out, err = t.ListDir(args.Path)
	case "read_file":
		out, err = t.ReadFile(args.Path)
	case "search_content":
		out, err = t.SearchContent(args.Query)
	default:
		return fmt.Sprintf("error: unknown tool %q", name)
	}
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return out
}

func displayPath(rel string) string {
	rel = strings.Trim(strings.TrimSpace(rel), "/")
	if rel == "" || rel == "." {
		return "/"
	}
	return "/" + rel
}
