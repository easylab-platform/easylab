package revision

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"easyvcs/internal/object"
)

// Status describes how a file changed between two trees.
type Status string

// File change statuses.
const (
	StatusAdded    Status = "added"
	StatusModified Status = "modified"
	StatusRemoved  Status = "removed"
)

// FileChange describes one file's change between two trees.
type FileChange struct {
	Path     string
	Status   Status
	OldID    object.ID
	NewID    object.ID
	Old, New []byte // content of removed/added (best-effort)
}

// DiffHunk represents a single contiguous block of changed lines within a
// modified file.
type DiffHunk struct {
	OldStart int // 1-based line in the old file where the hunk begins
	NewStart int // 1-based line in the new file where the hunk begins
	OldCount int // number of lines in the old file this hunk covers
	NewCount int // number of lines in the new file this hunk covers
	Lines    []DiffLine
}

// DiffLine is a single comparison line in a hunk.
type DiffLine struct {
	Kind string // "context", "del", "add"
	Text string
}

// UnifiedDiff computes a line-level unified diff between two blob contents.
// It returns nil if the contents are identical. This is a minimal Myers-style
// diff that is correct and readable without external dependencies.
func UnifiedDiff(old, new []byte) []DiffHunk {
	oldLines := splitLines(old)
	newLines := splitLines(new)
	// Quick equality check.
	if equalLines(oldLines, newLines) {
		return nil
	}
	ops := lcsOps(oldLines, newLines)
	return buildHunks(oldLines, newLines, ops)
}

func splitLines(data []byte) []string {
	s := string(data)
	if s == "" {
		// A single empty line preserves the "no newline" nuance crudely.
		return nil
	}
	lines := strings.Split(s, "\n")
	// Drop the trailing empty element produced by a terminal newline.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lcsOps returns per-line operations (keep/delete/add) via a simplified LCS.
// The result is the minimal edit script aligning oldLines to newLines.
func lcsOps(oldLines, newLines []string) []editOp {
	n, m := len(oldLines), len(newLines)
	// dp[i][j] = LCS length of old[:i], new[:j]
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if oldLines[i-1] == newLines[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else {
				if dp[i-1][j] >= dp[i][j-1] {
					dp[i][j] = dp[i-1][j]
				} else {
					dp[i][j] = dp[i][j-1]
				}
			}
		}
	}
	// Backtrack.
	var ops []editOp
	i, j := n, m
	for i > 0 && j > 0 {
		if oldLines[i-1] == newLines[j-1] {
			ops = append(ops, editOp{kind: opKeep, oldLine: oldLines[i-1], newLine: newLines[j-1]})
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			ops = append(ops, editOp{kind: opDel, oldLine: oldLines[i-1]})
			i--
		} else {
			ops = append(ops, editOp{kind: opAdd, newLine: newLines[j-1]})
			j--
		}
	}
	for i > 0 {
		ops = append(ops, editOp{kind: opDel, oldLine: oldLines[i-1]})
		i--
	}
	for j > 0 {
		ops = append(ops, editOp{kind: opAdd, newLine: newLines[j-1]})
		j--
	}
	// Reverse to get chronological order.
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

type editKind int

const (
	opKeep editKind = iota
	opDel
	opAdd
)

type editOp struct {
	kind    editKind
	oldLine string
	newLine string
}

// buildHunks groups the edit script into hunks with 3 lines of context either
// side, tracking old/new line numbers.
func buildHunks(oldLines, newLines []string, ops []editOp) []DiffHunk {
	const ctx = 3
	// First, annotate each op with the (oldLine, newLine) positions it consumes.
	type positioned struct {
		op     editOp
		oldIdx int // 0-based index into oldLines, or -1
		newIdx int // 0-based index into newLines, or -1
	}
	var pos []positioned
	oi, ni := 0, 0
	for _, op := range ops {
		switch op.kind {
		case opKeep:
			pos = append(pos, positioned{op: op, oldIdx: oi, newIdx: ni})
			oi++
			ni++
		case opDel:
			pos = append(pos, positioned{op: op, oldIdx: oi, newIdx: -1})
			oi++
		case opAdd:
			pos = append(pos, positioned{op: op, oldIdx: -1, newIdx: ni})
			ni++
		}
	}

	// Find indices of changed ops.
	var changes []int
	for i, p := range pos {
		if p.op.kind != opKeep {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return nil
	}

	// Group into hunks: consecutive changes (within 2*ctx gap) form one hunk.
	type group struct{ start, end int } // inclusive op indices
	var groups []group
	for _, c := range changes {
		if len(groups) == 0 || c-groups[len(groups)-1].end > 2*ctx {
			groups = append(groups, group{start: c, end: c})
		} else {
			g := &groups[len(groups)-1]
			if c > g.end {
				g.end = c
			}
		}
	}

	var hunks []DiffHunk
	for _, g := range groups {
		lo := g.start - ctx
		if lo < 0 {
			lo = 0
		}
		hi := g.end + ctx
		if hi >= len(pos) {
			hi = len(pos) - 1
		}
		var lines []DiffLine
		oldCount, newCount := 0, 0
		var oldStart, newStart int = -1, -1
		for i := lo; i <= hi; i++ {
			p := pos[i]
			switch p.op.kind {
			case opKeep:
				lines = append(lines, DiffLine{Kind: "context", Text: p.op.oldLine})
				oldCount++
				newCount++
				if oldStart < 0 {
					oldStart = p.oldIdx + 1
					newStart = p.newIdx + 1
				}
			case opDel:
				lines = append(lines, DiffLine{Kind: "del", Text: p.op.oldLine})
				oldCount++
				if oldStart < 0 {
					oldStart = p.oldIdx + 1
				}
			case opAdd:
				lines = append(lines, DiffLine{Kind: "add", Text: p.op.newLine})
				newCount++
				if newStart < 0 {
					newStart = p.newIdx + 1
				}
			}
		}
		// A hunk always has at least one side's start. Fall back to 1 if unknown.
		if oldStart < 0 {
			oldStart = 1
		}
		if newStart < 0 {
			newStart = 1
		}
		hunks = append(hunks, DiffHunk{
			OldStart: oldStart, NewStart: newStart,
			OldCount: oldCount, NewCount: newCount,
			Lines: lines,
		})
	}
	return hunks
}

// RenderUnified renders a UnifiedDiff as text with unified headers.
func RenderUnified(aName, bName string, hunks []DiffHunk) string {
	var b strings.Builder
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
		for _, ln := range h.Lines {
			switch ln.Kind {
			case "del":
				b.WriteString("-" + ln.Text + "\n")
			case "add":
				b.WriteString("+" + ln.Text + "\n")
			default:
				b.WriteString(" " + ln.Text + "\n")
			}
		}
	}
	return b.String()
}

// DiffContent computes a file-level unified diff between two trees, using the
// blob contents. It returns a list of per-file diffs (or nil for unchanged).
func (w *Workspace) DiffContent(a, b object.ID) ([]FileDiff, error) {
	changes, err := w.Diff(a, b)
	if err != nil {
		return nil, err
	}
	return w.DiffContentFromChanges(changes)
}

// DiffContentFromChanges renders pre-computed file changes as unified diffs.
func (w *Workspace) DiffContentFromChanges(changes []FileChange) ([]FileDiff, error) {
	var out []FileDiff
	for _, fc := range changes {
		fd := FileDiff{Path: fc.Path, Status: fc.Status}
		switch fc.Status {
		case StatusAdded:
			data, err := w.ReadBlob(fc.NewID)
			if err == nil {
				fd.Content = RenderUnified("/dev/null", fc.Path, UnifiedDiff(nil, data))
				fd.AddedLines = countLines(data)
			}
		case StatusRemoved:
			data, err := w.ReadBlob(fc.OldID)
			if err == nil {
				fd.Content = RenderUnified(fc.Path, "/dev/null", UnifiedDiff(data, nil))
				fd.RemovedLines = countLines(data)
			}
		case StatusModified:
			oldData, errO := w.ReadBlob(fc.OldID)
			newData, errN := w.ReadBlob(fc.NewID)
			if errO == nil && errN == nil {
				hunks := UnifiedDiff(oldData, newData)
				fd.Content = RenderUnified(fc.Path, fc.Path, hunks)
				for _, h := range hunks {
					for _, ln := range h.Lines {
						if ln.Kind == "add" {
							fd.AddedLines++
						} else if ln.Kind == "del" {
							fd.RemovedLines++
						}
					}
				}
			}
		}
		out = append(out, fd)
	}
	return out, nil
}

// FileDiff is a per-file line diff.
type FileDiff struct {
	Path         string
	Status       Status
	Content      string
	AddedLines   int
	RemovedLines int
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return bytes.Count(data, []byte("\n")) + 1
}

// SortFileDiffs orders file diffs by path.
func SortFileDiffs(ds []FileDiff) {
	sort.Slice(ds, func(i, j int) bool { return ds[i].Path < ds[j].Path })
}
