// Package ignore implements gitignore/.vcsignore handling.
//
// It layers a single gitignore matcher that honors both .gitignore and
// .vcsignore files at every directory, mirroring how jj/git exclude files from
// the tree. The matcher is path-based and supports the standard gitignore
// grammar (globs, directory-only, negation with `!`, nested files).
package ignore

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sabhiram/go-gitignore"
)

// FileNames are the ignore files read at each directory level.
var FileNames = []string{".gitignore", ".vcsignore"}

// Matcher answers whether a repo-relative path (forward slashes) should be
// excluded from a snapshot/build.
type Matcher struct {
	ignores []*ignore.GitIgnore
	dirs    []string // root-relative dir prefix for each ignore layer
}

// New loads ignore files starting at root, walking every subdirectory and
// building a layered matcher. Later (deeper) files take precedence.
func New(root string) (*Matcher, error) {
	m := &Matcher{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil || info.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		if !isIgnoreFile(name) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		// Load this file's patterns; MatchesPath expects the matcher to be
		// relative to the ignore file's directory. glob lines are relative.
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		m.ignores = append(m.ignores, ignore.CompileIgnoreLines(lines...))
		m.dirs = append(m.dirs, filepath.ToSlash(filepath.Dir(rel)))
		return nil
	})
	return m, err
}

// NewWithLines is a convenience for tests: build a matcher from a single set of
// patterns rooted at the given dir prefix (default ".").
func NewWithLines(prefix string, lines ...string) *Matcher {
	return &Matcher{ignores: []*ignore.GitIgnore{ignore.CompileIgnoreLines(lines...)}, dirs: []string{prefix}}
}

func isIgnoreFile(name string) bool {
	for _, f := range FileNames {
		if name == f {
			return true
		}
	}
	return false
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimRight(ln, "\r")
		out = append(out, ln)
	}
	return out, nil
}

// Ignored reports whether the repo-relative path (forward slashes) is ignored,
// applying each layer in order and honoring the last-match-wins semantics. A
// path is ignored if any layer matches it and no later layer explicitly
// un-ignores it.
func (m *Matcher) Ignored(rel string) bool {
	rel = filepath.ToSlash(rel)
	if m == nil {
		return false
	}
	result := false
	// Layers are walked root-first; deeper layers take precedence. A deeper
	// layer that mentions the path (positively or via negation) overrides the
	// accumulated result.
	for i, gi := range m.ignores {
		prefix := m.dirs[i]
		candidate := rel
		if prefix != "." && prefix != "" {
			if !strings.HasPrefix(candidate, prefix+"/") && candidate != prefix {
				continue
			}
			if candidate != prefix {
				candidate = strings.TrimPrefix(candidate, prefix+"/")
			} else {
				candidate = ""
			}
		}
		applies, ignored := layerApplies(gi, candidate)
		if applies {
			result = ignored
		}
	}
	return result
}

// HasAnyMatchUnder reports whether any ignore pattern could possibly match a
// path inside (or equal to) the given directory prefix. It is a conservative
// over-approximation: returning false guarantees no pattern applies to that
// subtree, so the caller can skip rewriting it altogether. Returning true does
// not mean a path matches — it only means the subtree must be inspected.
//
// A pattern compiled at layer dir L applies to paths under L. Therefore a
// directory `dir` can only be affected if some layer's L is an ancestor of, a
// descendant of, or equal to `dir`. A root layer (dir "." or "") matches
// everything and so always yields true.
func (m *Matcher) HasAnyMatchUnder(dir string) bool {
	if m == nil {
		return false
	}
	dir = filepath.ToSlash(dir)
	if dir == "" || dir == "." {
		dir = "."
	}
	for i := range m.ignores {
		L := m.dirs[i]
		if L == "" || L == "." {
			return true // root layer matches the whole tree
		}
		L = filepath.ToSlash(L)
		if dir == L ||
			strings.HasPrefix(dir, L+"/") ||
			strings.HasPrefix(L, dir+"/") {
			return true
		}
	}
	return false
}

// layerApplies reports whether a gitignore matcher mentions the candidate path
// at all (so it can override an outer layer), and whether the path ends up
// ignored after honoring negation within that file.
func layerApplies(gi *ignore.GitIgnore, candidate string) (applies, ignored bool) {
	matched, ip := gi.MatchesPathHow(candidate)
	_ = ip
	if !matched {
		return false, false
	}
	// Re-evaluate ignoring with full negation semantics.
	return true, gi.MatchesPath(candidate)
}
