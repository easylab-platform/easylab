package revision

import (
	"fmt"
)

// AnnotationLine attributes one line of a file to the revision that last
// introduced it, following the file's change history.
type AnnotationLine struct {
	LineNumber int    // 1-based line number in the current content
	RevisionID string // revision id that introduced this line
	Message    string // that revision's description
	Author     string // that revision's author
	Content    string // the line text
}

// Annotate renders the file at startID/path in the context of history, giving
// each line the revision that last touched it (line-level blame). A line that
// survives an edit keeps its original author; a line added by an edit is
// attributed to that edit's revision.
//
// startID is the starting revision id (empty picks the newest revision). It
// works by walking the chronological file edits and replaying each content diff
// to update per-line ownership using the same LCS used by UnifiedDiff.
func (w *Workspace) Annotate(startID, path string) ([]AnnotationLine, error) {
	return w.AnnotateAt(HistoryOpt{Start: startID}, path)
}

// AnnotateAt is the parameterized form of Annotate: it accepts a HistoryOpt so
// the starting revision can be a branch/tag name, revision id, "@", or empty.
func (w *Workspace) AnnotateAt(opt HistoryOpt, path string) ([]AnnotationLine, error) {
	edits, err := w.FileHistoryOpt(opt, path)
	if err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return nil, nil
	}

	// Collect each revision's blob content for this path, chronological (root
	// first). A removal yields a nil blob.
	type seg struct {
		rev    string
		msg    string
		author string
		blob   []byte
	}
	segs := make([]seg, 0, len(edits))
	for _, e := range edits {
		snap, err := w.store.GetSnapshot(e.RevisionHash)
		if err != nil {
			return nil, err
		}
		blobID, present, err := w.pathBlobHash(snap.TreeID, path)
		if err != nil {
			return nil, err
		}
		s := seg{rev: e.RevisionID, msg: snap.Description, author: snap.Author.String()}
		if present {
			data, err := w.ReadBlob(blobID)
			if err != nil {
				return nil, err
			}
			s.blob = data
		}
		segs = append(segs, s)
	}

	// Attribute lines to segments.
	var lines []string
	var owners []int
	cur := -1
	for i, s := range segs {
		newLines := splitLinesForAnnotate(s.blob)
		if cur == -1 {
			// First content-bearing segment seeds all lines.
			lines = newLines
			owners = make([]int, len(newLines))
			for k := range owners {
				owners[k] = i
			}
			cur = i
			continue
		}
		oldLines := splitLinesForAnnotate(segs[cur].blob)
		owners = w.replayOwnership(oldLines, owners, newLines, i)
		lines = newLines
		cur = i
	}

	if cur == -1 {
		return nil, nil
	}

	// Snapshot-attribution map: cur now points at the final content-bearing
	// segment, but owners still reference earlier segment indices, so use the
	// full segs slice.
	out := make([]AnnotationLine, len(lines))
	for i, ln := range lines {
		owner := owners[i]
		if owner < 0 || owner >= len(segs) {
			return nil, fmt.Errorf("annotate: invalid owner %d", owner)
		}
		s := segs[owner]
		out[i] = AnnotationLine{
			LineNumber: i + 1,
			RevisionID: s.rev,
			Message:    s.msg,
			Author:     s.author,
			Content:    ln,
		}
	}
	return out, nil
}

// replayOwnership applies the LCS transition from oldLines (with oldOwners) to
// newLines, attributing newly added lines to newOwner.
func (w *Workspace) replayOwnership(oldLines []string, oldOwners []int, newLines []string, newOwner int) []int {
	ops := lcsOps(oldLines, newLines)
	newOwners := make([]int, 0, len(newLines))
	oi, ni := 0, 0
	for _, op := range ops {
		switch op.kind {
		case opKeep:
			if oi < len(oldOwners) {
				newOwners = append(newOwners, oldOwners[oi])
			} else {
				newOwners = append(newOwners, newOwner)
			}
			oi++
			ni++
		case opDel:
			oi++
		case opAdd:
			newOwners = append(newOwners, newOwner)
			ni++
		}
	}
	return newOwners
}

func splitLinesForAnnotate(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return splitLines(data)
}
