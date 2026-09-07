package revision

import (
	"fmt"
	"strings"
	"time"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// FileEdit records one revision that modified a given file.
type FileEdit struct {
	RevisionID   string
	RevisionHash object.ID
	Timestamp    time.Time
	Status       Status // added, modified, or removed relative to its parent
}

// HistoryOpt controls file history traversal.
type HistoryOpt struct {
	// Start is a revision expression (branch name, tag name, revision id,
	// snapshot hash, "@", or "" for the newest revision). The walk begins at
	// the resolved revision and goes back through its ancestors.
	Start string
	// Desc orders the result newest-first (easylab-aligned). Default is
	// chronological (root first), matching the historical behaviour.
	Desc bool
	// Limit truncates the returned edits (0 = no limit).
	Limit int
}

// FileHistory returns the revisions (walking the DAG from the start revision
// upward to the root) that modified the given repo-relative path. A revision is
// included only when its ChangedPaths mentions the path and the blob hash
// actually differs from the previous revision that touched it.
//
// startID is a convenience that overrides opt.Start when non-empty. When
// opt.Desc is set the result is ordered newest-first; otherwise it is
// chronological (root-first).
func (w *Workspace) FileHistory(startID, path string) ([]FileEdit, error) {
	opt := HistoryOpt{Start: startID}
	return w.FileHistoryOpt(opt, path)
}

// FileHistoryOpt is the parameterized form of FileHistory.
func (w *Workspace) FileHistoryOpt(opt HistoryOpt, path string) ([]FileEdit, error) {
	revs, err := w.store.ListRevisions()
	if err != nil {
		return nil, err
	}
	// Build id -> revision and a parent index (snapshot hash -> revision id).
	byID := map[string]*store.Revision{}
	for _, r := range revs {
		byID[r.ID] = r
	}
	snapOwner := map[string]string{}
	for _, r := range revs {
		snapOwner[r.Hash.String()] = r.ID
	}

	start := opt.Start
	if start == "" {
		start = newestRevisionID(revs)
	} else {
		start = w.resolveStartID(start)
	}
	if start == "" {
		return nil, nil
	}

	// Walk ancestors topologically: start -> parents -> ... -> root.
	var chain []*store.Revision
	visited := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		r := byID[id]
		if r == nil {
			return
		}
		snap, err := w.store.GetSnapshot(r.Hash)
		if err != nil {
			return
		}
		// Recurse into the FIRST parent only so the walk stays on the mainline
		// (linear). A merge node counts as a single step; its secondary parents
		// are not expanded here (they are only visible via /graph).
		if len(snap.Parents) > 0 {
			if owner, ok := snapOwner[snap.Parents[0].String()]; ok {
				walk(owner)
			}
		}
		chain = append(chain, r)
	}
	walk(start)

	// Walk the chain in chronological order; track the last blob hash seen for
	// the path so we count only actual content changes.
	var edits []FileEdit
	var lastID object.ID
	haveLast := false
	for _, r := range chain {
		mentions := false
		for _, cp := range r.ChangedPaths {
			if cp == path {
				mentions = true
				break
			}
		}
		if !mentions {
			continue
		}
		snap, err := w.store.GetSnapshot(r.Hash)
		if err != nil {
			continue
		}
		blobID, present, err := w.pathBlobHash(snap.TreeID, path)
		if err != nil {
			continue
		}
		status := StatusModified
		if !haveLast {
			status = StatusAdded
		} else if !present {
			status = StatusRemoved
		} else if blobID == lastID {
			continue // content unchanged (e.g. amend touching metadata)
		}
		edits = append(edits, FileEdit{
			RevisionID: r.ID, RevisionHash: r.Hash,
			Timestamp: snap.CommitTime, Status: status,
		})
		if present {
			lastID = blobID
			haveLast = true
		}
	}

	if opt.Desc {
		reverseFileEdits(edits)
	}
	if opt.Limit > 0 && len(edits) > opt.Limit {
		edits = edits[:opt.Limit]
	}
	return edits, nil
}

// resolveStartID converts a raw Start expression to a concrete revision id. It
// accepts branch/tag names, revision ids, snapshot hashes, "@", or "".
func (w *Workspace) resolveStartID(expr string) string {
	if expr == "" {
		return ""
	}
	if expr == "@" {
		return w.autoStartRevision()
	}
	if ref, err := w.GetRef(expr); err == nil && ref != nil {
		return ref.Target
	}
	if id, err := w.revisionIDFor(expr); err == nil {
		return id
	}
	return expr
}

// revisionIDFor maps a snapshot hash or revision id prefix to a revision id.
func (w *Workspace) revisionIDFor(ref string) (string, error) {
	revs, err := w.store.ListRevisions()
	if err != nil {
		return "", err
	}
	for _, r := range revs {
		if r.ID == ref {
			return r.ID, nil
		}
	}
	for _, r := range revs {
		if strings.HasPrefix(r.ID, ref) {
			return r.ID, nil
		}
	}
	// Snapshot hash lookup.
	for _, r := range revs {
		if r.Hash.String() == ref {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("unknown ref %q", ref)
}

// autoStartRevision returns the newest revision id.
func (w *Workspace) autoStartRevision() string {
	revs, err := w.store.ListRevisions()
	if err != nil {
		return ""
	}
	return newestRevisionID(revs)
}

func reverseFileEdits(edits []FileEdit) {
	for i, j := 0, len(edits)-1; i < j; i, j = i+1, j-1 {
		edits[i], edits[j] = edits[j], edits[i]
	}
}

// pathBlobHash returns the blob id present at a path in a tree, plus whether
// the path exists.
func (w *Workspace) pathBlobHash(treeID object.ID, path string) (object.ID, bool, error) {
	entry, err := w.findEntry(w.mustTree(treeID), path)
	if err != nil {
		return object.ID{}, false, nil
	}
	if entry.Kind != object.KindBlob {
		return object.ID{}, false, nil
	}
	return entry.ID, true, nil
}

func newestRevisionID(revs []*store.Revision) string {
	if len(revs) == 0 {
		return ""
	}
	newest := revs[0]
	for _, r := range revs[1:] {
		if r.Created.After(newest.Created) {
			newest = r
		}
	}
	return newest.ID
}
