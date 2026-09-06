package revision

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"easyvcs/internal/merge"
	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// joinPath joins path components with forward slashes.
func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "/" + child
}

// writeTree stores a tree object and returns its id.
func (w *Workspace) writeTree(t *object.Tree) object.ID {
	o := &object.Object{Kind: object.KindTree, Tree: t}
	_ = w.store.WriteObject(o)
	return o.ID()
}

// WriteTree stores a tree object and returns its id (exported).
func (w *Workspace) WriteTree(t *object.Tree) object.ID { return w.writeTree(t) }

// MustTree reads a tree by id, returning an empty tree on error (exported).
func (w *Workspace) MustTree(id object.ID) *object.Tree { return w.mustTree(id) }

// FindEntry walks a tree down to the entry at path (exported).
func (w *Workspace) FindEntry(t *object.Tree, path string) (object.Entry, error) {
	return w.findEntry(t, path)
}

// mustTree reads a tree or falls back to an empty tree.
func (w *Workspace) mustTree(id object.ID) *object.Tree {
	t, err := w.ReadTree(id)
	if err != nil {
		return object.NewTree()
	}
	return t
}

// ReadBlob reads a blob's contents by id.
func (w *Workspace) ReadBlob(id object.ID) ([]byte, error) {
	o, err := w.store.ReadObject(id)
	if err != nil {
		return nil, err
	}
	if o.Kind != object.KindBlob {
		return nil, fmt.Errorf("object %s is not a blob", id)
	}
	return o.Blob, nil
}

// WriteBlob writes a blob object and returns its id.
func (w *Workspace) WriteBlob(data []byte) object.ID {
	o := &object.Object{Kind: object.KindBlob, Blob: data}
	_ = w.store.WriteObject(o)
	return o.ID()
}

// treeOps adapter satisfies merge.treeOps.

// WriteConflict implements merge.treeOps.
func (w *Workspace) WriteConflict(c *object.Conflict) object.ID {
	o := &object.Object{Kind: object.KindConflict, Conflict: c}
	_ = w.store.WriteObject(o)
	return o.ID()
}

// Rebase repoints a revision onto a single parent, re-creating its snapshot
// with a new revision_hash while keeping the same revision id. The target tree
// is recomputed via a 3-way merge so the revision's own diff is genuinely
// re-applied onto the new base (not merely re-parented). Overlapping edits are
// recorded as first-class conflict objects in the resulting tree.
//
// newParents must be exactly one snapshot hash. Multi-parent rebase is not a
// supported operation (the model is single-parent / linear).
func (w *Workspace) Rebase(revisionID string, newParents []object.ID) (*store.Snapshot, *store.Revision, error) {
	if len(newParents) != 1 {
		return nil, nil, fmt.Errorf("rebase: expected exactly one new parent, got %d", len(newParents))
	}
	rev, err := w.store.GetRevision(revisionID)
	if err != nil {
		return nil, nil, ErrRevisionNotFound
	}
	cur, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, nil, err
	}

	newBase, err := w.store.GetSnapshot(newParents[0])
	if err != nil {
		return nil, nil, fmt.Errorf("rebase: new parent snapshot not found: %w", err)
	}

	// The 3-way base is the revision's original first parent tree (the point
	// at which it diverged). A root revision uses the empty tree as its base.
	var baseTree object.ID
	if len(cur.Parents) > 0 {
		if pSnap, err := w.store.GetSnapshot(cur.Parents[0]); err == nil {
			baseTree = pSnap.TreeID
		}
	}
	// Merge(base, ours=cur, theirs=newBase) re-applies the revision's own
	// delta (ours) onto the new base (theirs); overlapping edits become
	// conflict objects. "ours" is kept as the added side so a resolve to side 0
	// prefers the rebased revision's own content.
	mergedTree, _, err := w.Merge(baseTree, cur.TreeID, newBase.TreeID)
	if err != nil {
		return nil, nil, err
	}

	ns := &store.Snapshot{
		RevisionID:  revisionID,
		Parents:     newParents, // single parent
		TreeID:      mergedTree,
		Description: cur.Description,
		Author:      cur.Author,
		CommitTime:  cur.CommitTime,
	}
	ns.RevisionHash = snapshotID(ns)
	if err := w.store.PutSnapshot(ns); err != nil {
		return nil, nil, err
	}
	if err := w.store.UpdateRevisionHash(revisionID, ns.RevisionHash); err != nil {
		return nil, nil, err
	}
	rev.Hash = ns.RevisionHash
	rev.ChangedPaths = w.changedPathsBetween(ns.Parents, ns.RevisionHash)
	if err := w.store.PutRevision(rev); err != nil {
		return nil, nil, err
	}
	return ns, rev, nil
}

// RebaseMany moves a set of revisions onto a new base in one chained operation,
// re-parenting them in ancestor order so the result is a linear sequence. It is
// the multi-revision convenience over Rebase: for example, rebasing [D, H]
// (with chain B-D-G-H) onto F yields A-B-C-E-F-D-H, dropping G. Revisions not
// in the set (e.g. G) become unreachable and are effectively discarded.
//
// revs are revision ids in the order they should appear on the new line (tip
// last). Every moved revision keeps its own id and has its tree recomputed via
// 3-way merge; overlapping edits become first-class conflict objects.
func (w *Workspace) RebaseMany(revs []string, onto object.ID) ([]*store.Revision, error) {
	if len(revs) == 0 {
		return nil, nil
	}
	parent := onto
	var moved []*store.Revision
	for _, rid := range revs {
		// Rebase onto the previous node in the sequence (or the given onto).
		ns, ch, err := w.Rebase(rid, []object.ID{parent})
		if err != nil {
			return moved, err
		}
		_ = ns
		moved = append(moved, ch)
		parent = ch.Hash
	}
	return moved, nil
}

// DescendantsOf returns the revision ids (in ancestor-first order) that are
// reachable from revID by following the FIRST parent chain downwards. In the
// linear/single-parent model this is simply the chain of commits that come
// after revID. The result excludes revID itself.
func (w *Workspace) DescendantsOf(revID string) ([]string, error) {
	rev, err := w.store.GetRevision(revID)
	if err != nil {
		return nil, err
	}
	snap, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, err
	}
	// Build parent-hash -> child revisions (first-parent edges only).
	children := map[string][]string{}
	revs, err := w.store.ListRevisions()
	if err != nil {
		return nil, err
	}
	for _, r := range revs {
		if r.Hash == rev.Hash {
			continue
		}
		rs, err := w.store.GetSnapshot(r.Hash)
		if err != nil {
			continue
		}
		if len(rs.Parents) > 0 {
			children[rs.Parents[0].String()] = append(children[rs.Parents[0].String()], r.ID)
		}
	}
	// BFS from rev's snapshot hash, collecting in topological (ancestor-first)
	// order so the descendants can be re-parented sequentially.
	var out []string
	seen := map[string]bool{}
	frontier := []string{snap.RevisionHash.String()}
	for len(frontier) > 0 {
		var next []string
		for _, h := range frontier {
			for _, cid := range children[h] {
				if seen[cid] {
					continue
				}
				seen[cid] = true
				out = append(out, cid)
				cs, err := w.store.GetSnapshot(revisionHashOf(cid, w))
				if err != nil {
					continue
				}
				next = append(next, cs.RevisionHash.String())
			}
		}
		frontier = next
	}
	return out, nil
}

// Drop removes a revision from history by rebasing all of its descendants onto
// the revision's parent, then re-pointing them so the dropped revision becomes
// unreachable. It is the "erase from history" operation (no op-log preserved).
// Returns the moved descendant revisions.
func (w *Workspace) Drop(revID string) ([]*store.Revision, error) {
	rev, err := w.store.GetRevision(revID)
	if err != nil {
		return nil, ErrRevisionNotFound
	}
	snap, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, err
	}
	if len(snap.Parents) == 0 {
		// Dropping the root: no parent to rebase onto; refuse.
		return nil, fmt.Errorf("drop: cannot drop a root revision")
	}
	parentSnap, err := w.store.GetSnapshot(snap.Parents[0])
	if err != nil {
		return nil, err
	}
	desc, err := w.DescendantsOf(revID)
	if err != nil {
		return nil, err
	}
	return w.RebaseMany(desc, parentSnap.RevisionHash)
}

// Revert creates a new revision that undoes the changes introduced by revID,
// applied on top of onto (default: the current tip). The historical revision is
// preserved; this is the inverse-patch model (like `git revert`), distinct from
// Drop which erases the revision. Conflicts appear as first-class objects.
func (w *Workspace) Revert(revID string, onto object.ID) (*store.Snapshot, *store.Revision, error) {
	rev, err := w.store.GetRevision(revID)
	if err != nil {
		return nil, nil, ErrRevisionNotFound
	}
	revSnap, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, nil, err
	}
	// The inverse base: the revision's parent tree (undoing rev's diff).
	var inverseBase object.ID
	if len(revSnap.Parents) > 0 {
		p, err := w.store.GetSnapshot(revSnap.Parents[0])
		if err != nil {
			return nil, nil, err
		}
		inverseBase = p.TreeID
	} else {
		inverseBase = object.ID{} // root: undoing it means empty tree
	}

	// Find the target to revert onto (current tip if onto is zero).
	var ontoSnap *store.Snapshot
	if onto.IsZero() {
		revs, err := w.store.ListRevisions()
		if err != nil || len(revs) == 0 {
			return nil, nil, fmt.Errorf("revert: no revisions")
		}
		newest := newestRevisionID(revs)
		r, err := w.store.GetRevision(newest)
		if err != nil {
			return nil, nil, err
		}
		ontoSnap, err = w.store.GetSnapshot(r.Hash)
		if err != nil {
			return nil, nil, err
		}
	} else {
		ontoSnap, err = w.store.GetSnapshot(onto)
		if err != nil {
			return nil, nil, fmt.Errorf("revert: onto snapshot not found: %w", err)
		}
	}

	// 3-way: base = rev's tree, ours = rev's parent tree (undo), theirs = onto.
	// Overlaps (where onto diverges from the reverted content) become conflicts.
	mergedTree, _, err := w.Merge(revSnap.TreeID, inverseBase, ontoSnap.TreeID)
	if err != nil {
		return nil, nil, err
	}

	return w.Commit(CommitParams{
		Parents:     []object.ID{ontoSnap.RevisionHash},
		TreeID:      mergedTree,
		Description: "Revert " + revID,
		Author:      store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
	})
}

// revisionHashOf resolves a revision id to its current snapshot hash.
func revisionHashOf(revID string, w *Workspace) object.ID {
	r, err := w.store.GetRevision(revID)
	if err != nil {
		return object.ID{}
	}
	return r.Hash
}

// Merge performs a 3-way merge of the trees of two snapshots, recording any
// conflicts as first-class objects in the resulting tree.
func (w *Workspace) Merge(base, ours, theirs object.ID) (object.ID, []merge.ConflictAtom, error) {
	baseTree := w.mustTree(base)
	oursTree := w.mustTree(ours)
	theirsTree := w.mustTree(theirs)
	merged, atoms := merge.Trees(baseTree, oursTree, theirsTree, w)
	return w.writeTree(merged), atoms, nil
}

// Resolve replaces the conflict at the given path in a revision's current tree
// with a chosen side, keeping the same revision id. Resolution propagates to
// descendant revisions that still carry the same conflict.
func (w *Workspace) Resolve(revisionID string, path string, sideIndex int) (*store.Snapshot, *store.Revision, error) {
	rev, err := w.store.GetRevision(revisionID)
	if err != nil {
		return nil, nil, ErrRevisionNotFound
	}
	cur, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, nil, err
	}
	entry, err := w.findEntry(w.mustTree(cur.TreeID), path)
	if err != nil {
		return nil, nil, err
	}
	if entry.Kind != object.KindConflict {
		return nil, nil, fmt.Errorf("no conflict at path %s", path)
	}
	conflictObj, err := w.store.ReadObject(entry.ID)
	if err != nil {
		return nil, nil, err
	}
	if conflictObj.Kind != object.KindConflict || conflictObj.Conflict == nil {
		return nil, nil, fmt.Errorf("path %s does not hold a conflict", path)
	}
	if sideIndex < 0 || sideIndex >= len(conflictObj.Conflict.Adds) {
		return nil, nil, fmt.Errorf("side index %d out of range (0..%d)", sideIndex, len(conflictObj.Conflict.Adds)-1)
	}

	term := conflictObj.Conflict.Adds[sideIndex]
	var chosen object.ID
	remove := term.ID == (object.ID{})
	if !remove {
		chosen = term.ID
	}

	descendants, err := w.descendantsFromSnapshot(cur.RevisionHash)
	if err != nil {
		return nil, nil, err
	}

	newTreeID, err := w.resolvePath(w.mustTree(cur.TreeID), path, chosen, remove)
	if err != nil {
		return nil, nil, err
	}
	ns := w.newSnapshotFor(revisionID, cur, newTreeID)
	if err := w.store.PutSnapshot(ns); err != nil {
		return nil, nil, err
	}
	if err := w.store.UpdateRevisionHash(revisionID, ns.RevisionHash); err != nil {
		return nil, nil, err
	}

	propagated := 0
	for _, d := range descendants {
		curD, err := w.store.GetSnapshot(d.Hash)
		if err != nil {
			continue
		}
		treeD := w.mustTree(curD.TreeID)
		dEntry, err := w.findEntry(treeD, path)
		if err != nil {
			continue
		}
		if dEntry.Kind != object.KindConflict || dEntry.ID != entry.ID {
			continue
		}
		newTreeD, err := w.resolvePath(treeD, path, chosen, remove)
		if err != nil {
			return nil, nil, err
		}
		nsD := w.newSnapshotFor(d.ID, curD, newTreeD)
		if err := w.store.PutSnapshot(nsD); err != nil {
			return nil, nil, err
		}
		if err := w.store.UpdateRevisionHash(d.ID, nsD.RevisionHash); err != nil {
			return nil, nil, err
		}
		propagated++
	}
	_ = propagated
	return ns, rev, nil
}

// newSnapshotFor builds a new snapshot for a revision with the given tree,
// keeping the revision's other metadata and id.
func (w *Workspace) newSnapshotFor(revisionID string, cur *store.Snapshot, treeID object.ID) *store.Snapshot {
	ns := &store.Snapshot{
		RevisionID:  revisionID,
		Parents:     cur.Parents,
		TreeID:      treeID,
		Description: cur.Description,
		Author:      cur.Author,
		CommitTime:  cur.CommitTime,
	}
	ns.RevisionHash = snapshotID(ns)
	return ns
}

// descendantsFromSnapshot returns all revisions reachable by following parent
// edges forward from the given snapshot hash. The revision that owns the seed
// snapshot is excluded.
func (w *Workspace) descendantsFromSnapshot(seed object.ID) ([]*store.Revision, error) {
	all, err := w.store.ListRevisions()
	if err != nil {
		return nil, err
	}
	snapOwner := map[string]string{}
	for _, rv := range all {
		snapOwner[rv.Hash.String()] = rv.ID
	}
	owner := snapOwner[seed.String()]
	visited := map[string]bool{}
	if owner != "" {
		visited[owner] = true
	}
	queue := []object.ID{seed}
	seenSnaps := map[object.ID]bool{seed: true}
	var out []*store.Revision
	for len(queue) > 0 {
		snapID := queue[0]
		queue = queue[1:]
		for _, cand := range all {
			candSnap, err := w.store.GetSnapshot(cand.Hash)
			if err != nil {
				continue
			}
			for _, p := range candSnap.Parents {
				if p == snapID {
					if !visited[cand.ID] {
						visited[cand.ID] = true
						out = append(out, cand)
					}
					if !seenSnaps[candSnap.RevisionHash] {
						seenSnaps[candSnap.RevisionHash] = true
						queue = append(queue, candSnap.RevisionHash)
					}
					break
				}
			}
		}
	}
	return out, nil
}

// ConflictsInTree returns the conflict atoms embedded in a tree.
func (w *Workspace) ConflictsInTree(treeID object.ID) ([]merge.ConflictAtom, error) {
	var out []merge.ConflictAtom
	if err := w.collectConflicts("", w.mustTree(treeID), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (w *Workspace) collectConflicts(prefix string, t *object.Tree, out *[]merge.ConflictAtom) error {
	for _, e := range t.SortedEntries() {
		path := joinPath(prefix, e.Name)
		if e.Kind == object.KindConflict {
			*out = append(*out, merge.ConflictAtom{Path: path, ID: e.ID})
			continue
		}
		if e.Kind == object.KindTree {
			sub, err := w.ReadTree(e.ID)
			if err != nil {
				continue
			}
			if err := w.collectConflicts(path, sub, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// Squash absorbs the changes in a revision into its parent revision, keeping
// the parent revision's id stable.
func (w *Workspace) Squash(childRevisionID string) (*store.Snapshot, *store.Revision, error) {
	child, err := w.store.GetRevision(childRevisionID)
	if err != nil {
		return nil, nil, ErrRevisionNotFound
	}
	childSnap, err := w.store.GetSnapshot(child.Hash)
	if err != nil {
		return nil, nil, err
	}
	if len(childSnap.Parents) == 0 {
		return nil, nil, fmt.Errorf("child revision has no parent to squash into")
	}
	parentSnap, err := w.store.GetSnapshot(childSnap.Parents[0])
	if err != nil {
		return nil, nil, err
	}
	parentRev, err := w.store.GetRevision(parentSnap.RevisionID)
	if err != nil {
		return nil, nil, err
	}
	merged, _, err := w.Merge(parentSnap.TreeID, parentSnap.TreeID, childSnap.TreeID)
	if err != nil {
		return nil, nil, err
	}
	ns := &store.Snapshot{
		RevisionID:  parentRev.ID,
		Parents:     parentSnap.Parents,
		TreeID:      merged,
		Description: parentSnap.Description,
		Author:      parentSnap.Author,
	}
	ns.RevisionHash = snapshotID(ns)
	if err := w.store.PutSnapshot(ns); err != nil {
		return nil, nil, err
	}
	if err := w.store.UpdateRevisionHash(parentRev.ID, ns.RevisionHash); err != nil {
		return nil, nil, err
	}
	return ns, parentRev, nil
}

// SetRef creates or updates a branch/tag pointing to a revision.
func (w *Workspace) SetRef(name string, kind store.RefKind, revisionID string) (*store.Ref, error) {
	r := &store.Ref{Name: name, Kind: kind, Target: revisionID}
	if err := w.store.PutRef(r); err != nil {
		return nil, err
	}
	return r, nil
}

// DeleteRef removes a ref.
func (w *Workspace) DeleteRef(name string) error { return w.store.DeleteRef(name) }

// GetRef returns a ref by name.
func (w *Workspace) GetRef(name string) (*store.Ref, error) { return w.store.GetRef(name) }

// ListRefs returns all refs.
func (w *Workspace) ListRefs() ([]*store.Ref, error) { return w.store.ListRefs() }

// Diff walks two trees and lists per-file differences.
func (w *Workspace) Diff(a, b object.ID) ([]FileChange, error) {
	ta := w.mustTree(a)
	tb := w.mustTree(b)
	var out []FileChange
	if err := w.diffTree("", ta, tb, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// DiffBaseForRevision returns the tree to diff a revision against, always the
// first parent's tree (root -> empty tree). The model is single-parent/linear:
// every revision has exactly one parent, so the diff base is unambiguous.
func (w *Workspace) DiffBaseForRevision(revisionID string) (object.ID, error) {
	rev, err := w.store.GetRevision(revisionID)
	if err != nil {
		return object.ID{}, err
	}
	snap, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return object.ID{}, err
	}
	if len(snap.Parents) == 0 {
		return object.ID{}, nil // root
	}
	parent, err := w.store.GetSnapshot(snap.Parents[0])
	if err != nil {
		return object.ID{}, err
	}
	return parent.TreeID, nil
}

// FilesChanged returns the per-file change list for a revision, expressed
// against its first-parent base (root -> empty). This is the "which files did
// this revision change, and how" view.
func (w *Workspace) FilesChanged(revisionID string) ([]FileChange, error) {
	rev, err := w.store.GetRevision(revisionID)
	if err != nil {
		return nil, err
	}
	snap, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, err
	}
	base, err := w.DiffBaseForRevision(revisionID)
	if err != nil {
		return nil, err
	}
	return w.Diff(base, snap.TreeID)
}

func (w *Workspace) diffTree(prefix string, a, b *object.Tree, out *[]FileChange) error {
	names := map[string]struct{}{}
	for _, t := range []*object.Tree{a, b} {
		for n := range t.Entries {
			names[n] = struct{}{}
		}
	}
	all := make([]string, 0, len(names))
	for n := range names {
		all = append(all, n)
	}
	sort.Strings(all)
	for _, name := range all {
		ea, aHas := a.Entries[name]
		eb, bHas := b.Entries[name]
		path := joinPath(prefix, name)
		aTree, aIsTree := w.readTreeEntryOrNil(ea, aHas)
		bTree, bIsTree := w.readTreeEntryOrNil(eb, bHas)
		switch {
		case aIsTree && bIsTree:
			if err := w.diffTree(path, aTree, bTree, out); err != nil {
				return err
			}
		case aIsTree && !bIsTree:
			w.flattenTree(path, aTree, out, StatusRemoved)
		case !aIsTree && bIsTree:
			w.flattenTree(path, bTree, out, StatusAdded)
		default:
			aBlob := aHas && ea.Kind == object.KindBlob
			bBlob := bHas && eb.Kind == object.KindBlob
			switch {
			case aBlob && bBlob && ea.ID == eb.ID:
			case aBlob && bBlob:
				*out = append(*out, FileChange{Path: path, Status: StatusModified, OldID: ea.ID, NewID: eb.ID})
			case aBlob && !bBlob:
				*out = append(*out, FileChange{Path: path, Status: StatusRemoved, OldID: ea.ID})
			case !aBlob && bBlob:
				*out = append(*out, FileChange{Path: path, Status: StatusAdded, NewID: eb.ID})
			}
		}
	}
	return nil
}

func (w *Workspace) flattenTree(prefix string, t *object.Tree, out *[]FileChange, status Status) {
	for _, e := range t.SortedEntries() {
		path := joinPath(prefix, e.Name)
		if sub, isTree := w.readTreeEntry(e); isTree {
			w.flattenTree(path, sub, out, status)
			continue
		}
		fc := FileChange{Path: path, Status: status}
		if status == StatusRemoved {
			fc.OldID = e.ID
		} else {
			fc.NewID = e.ID
		}
		*out = append(*out, fc)
	}
}

func (w *Workspace) readTreeEntry(e object.Entry) (*object.Tree, bool) {
	if e.Kind != object.KindTree {
		return nil, false
	}
	t, err := w.ReadTree(e.ID)
	if err != nil {
		return nil, false
	}
	return t, true
}

func (w *Workspace) readTreeEntryOrNil(e object.Entry, present bool) (*object.Tree, bool) {
	if !present {
		return nil, false
	}
	return w.readTreeEntry(e)
}

// Materialize writes a tree's contents into a directory, rendering conflicts
// as marker files.
func (w *Workspace) Materialize(treeID object.ID, dest string) error {
	return w.materializeTree(w.mustTree(treeID), dest)
}

func (w *Workspace) materializeTree(t *object.Tree, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, e := range t.SortedEntries() {
		full := filepath.Join(dir, e.Name)
		if sub, isTree := w.readTreeEntry(e); isTree {
			if err := w.materializeTree(sub, full); err != nil {
				return err
			}
			continue
		}
		data, err := w.store.ReadObject(e.ID)
		if err != nil {
			return err
		}
		if data.Kind == object.KindConflict {
			text, err := w.renderConflict(e.ID)
			if err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
				return err
			}
			continue
		}
		if data.Kind != object.KindBlob {
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(full, data.Blob, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// renderConflict produces conflict-marker text for a conflict object.
func (w *Workspace) renderConflict(conflictID object.ID) (string, error) {
	o, err := w.store.ReadObject(conflictID)
	if err != nil {
		return "", err
	}
	if o.Kind != object.KindConflict || o.Conflict == nil {
		return "", fmt.Errorf("object %s is not a conflict", conflictID)
	}
	c := o.Conflict
	var b strings.Builder
	b.WriteString(fmt.Sprintf("<<<<<<< conflict (%d terms)\n", c.Numeric()))
	if len(c.Adds) > 0 {
		first, err := w.blobText(c.Adds[0].ID)
		if err != nil {
			return "", err
		}
		b.WriteString(first)
		b.WriteString("\n")
	}
	for i := 1; i < len(c.Adds); i++ {
		label := c.Adds[i].Label
		b.WriteString("%%%%%%% side #" + fmt.Sprint(i) + " (" + label + ")\n")
		text, err := w.blobText(c.Adds[i].ID)
		if err != nil {
			return "", err
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	for _, term := range c.Removes {
		b.WriteString("%%%%%%% diff from: " + term.Label + "\n")
		text, err := w.blobText(term.ID)
		if err != nil {
			return "", err
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	b.WriteString(">>>>>>> conflict ends\n")
	return b.String(), nil
}

// blobText returns text content of a blob, or "" for the absent id.
func (w *Workspace) blobText(id object.ID) (string, error) {
	if id == (object.ID{}) {
		return "", nil
	}
	o, err := w.store.ReadObject(id)
	if err != nil {
		return "", err
	}
	if o.Kind != object.KindBlob {
		return "", fmt.Errorf("object %s is not a blob", id)
	}
	return string(o.Blob), nil
}

// findEntry walks a tree down to the entry at path.
func (w *Workspace) findEntry(t *object.Tree, path string) (object.Entry, error) {
	var components []string
	cur := path
	for {
		idx := strings.IndexByte(cur, '/')
		if idx < 0 {
			components = append(components, cur)
			break
		}
		components = append(components, cur[:idx])
		cur = cur[idx+1:]
	}
	entry, ok := t.Entries[components[0]]
	if !ok {
		return object.Entry{}, fmt.Errorf("no entry at path %s", path)
	}
	for i := 1; i < len(components); i++ {
		if entry.Kind != object.KindTree {
			return object.Entry{}, fmt.Errorf("path %s traverses a non-tree", path)
		}
		sub, err := w.ReadTree(entry.ID)
		if err != nil {
			return object.Entry{}, err
		}
		entry, ok = sub.Entries[components[i]]
		if !ok {
			return object.Entry{}, fmt.Errorf("no entry at path %s", path)
		}
	}
	return entry, nil
}

// resolvePath replaces the entry at path in a tree with a blob (or removes it).
func (w *Workspace) resolvePath(t *object.Tree, path string, chosen object.ID, remove bool) (object.ID, error) {
	var components []string
	cur := path
	for {
		idx := strings.IndexByte(cur, '/')
		if idx < 0 {
			components = append(components, cur)
			break
		}
		components = append(components, cur[:idx])
		cur = cur[idx+1:]
	}
	var rewrite func(t *object.Tree, idx int) (*object.Tree, error)
	rewrite = func(t *object.Tree, idx int) (*object.Tree, error) {
		if idx == len(components) {
			return t, nil
		}
		name := components[idx]
		child, ok := t.Entries[name]
		clone := object.NewTree()
		for _, e := range t.SortedEntries() {
			clone.Entries[e.Name] = e
		}
		if idx == len(components)-1 {
			if remove {
				delete(clone.Entries, name)
			} else {
				clone.Entries[name] = object.Entry{Name: name, Kind: object.KindBlob, ID: chosen}
			}
			return clone, nil
		}
		// Intermediate dir: recurse into existing child subtree, or create a new
		// empty subtree if the path doesn't exist yet (for adding nested files).
		var childTree *object.Tree
		if ok && child.Kind == object.KindTree {
			childTree = w.mustTree(child.ID)
		} else {
			childTree = object.NewTree()
		}
		newChild, err := rewrite(childTree, idx+1)
		if err != nil {
			return nil, err
		}
		if len(newChild.Entries) == 0 && remove {
			delete(clone.Entries, name)
		} else {
			clone.Entries[name] = object.Entry{Name: name, Kind: object.KindTree, ID: w.writeTree(newChild)}
		}
		return clone, nil
	}
	newTree, err := rewrite(t, 0)
	if err != nil {
		return object.ID{}, err
	}
	return w.writeTree(newTree), nil
}
