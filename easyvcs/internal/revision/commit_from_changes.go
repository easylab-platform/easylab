package revision

import (
	"errors"
	"time"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// FileChangeSpec describes one file modification for an atomic commit.
type FileChangeSpec struct {
	// Path is the repo-relative path (forward slashes).
	Path string
	// Content, if set and Delete is false, writes the given content to the path
	// (creating or replacing the file).
	Content []byte
	// Delete removes the path if true. Content is ignored.
	Delete bool
}

// CommitFromChanges atomically creates a revision from a set of file changes
// applied on top of a parent snapshot. It builds the new tree by starting from
// the parent's tree (or an empty tree for a root commit) and applying each
// change, then writes the snapshot and revision in a single database
// transaction so the commit is atomic.
//
// A new revision_id is generated unless revisionIDOverride is non-empty (used
// by amend to keep the stable id). parents is the single parent_hash (or empty
// for a root commit). ChangedPaths is set to the changed paths. If parent_hash
// is non-empty but does not exist in this repository, an error is returned.
func (w *Workspace) CommitFromChanges(parentHash object.ID, changes []FileChangeSpec, description string, author store.Author, revisionIDOverride string) (*store.Snapshot, *store.Revision, error) {
	revisionID := revisionIDOverride
	if revisionID == "" {
		var err error
		revisionID, err = object.RandomChangeID()
		if err != nil {
			return nil, nil, err
		}
	}

	var baseTree *object.Tree
	var parents []object.ID
	if parentHash != (object.ID{}) {
		parentSnap, err := w.store.GetSnapshot(parentHash)
		if err != nil {
			return nil, nil, errors.New("parent snapshot not found")
		}
		baseTree = w.mustTree(parentSnap.TreeID)
		parents = []object.ID{parentHash}
	} else {
		baseTree = object.NewTree()
	}

	// Amend (revisionIDOverride != ""): keep the revision's existing parent
	// pointers (1:1, replace-in-place) while applying the new content on top of
	// the parent snapshot. Without this, an amended snapshot would point at its
	// own previous snapshot as a parent.
	if revisionIDOverride != "" {
		if old, err := w.store.GetRevision(revisionIDOverride); err == nil {
			if oldSnap, err := w.store.GetSnapshot(old.Hash); err == nil {
				parents = oldSnap.Parents
			}
		}
	}

	// Build the changed blob objects and track changed paths.
	changedPaths := make([]string, 0, len(changes))
	// Apply each change to the working tree by rewriting the base tree.
	newTree := baseTree
	for _, c := range changes {
		changedPaths = append(changedPaths, c.Path)
		if c.Delete {
			tid, err := w.resolvePath(newTree, c.Path, object.ID{}, true)
			if err != nil {
				return nil, nil, err
			}
			newTree = w.mustTree(tid)
			continue
		}
		blobID := w.WriteBlob(c.Content)
		tid, err := w.resolvePath(newTree, c.Path, blobID, false)
		if err != nil {
			return nil, nil, err
		}
		newTree = w.mustTree(tid)
	}

	snap := &store.Snapshot{
		RevisionID:  revisionID,
		Parents:     parents,
		TreeID:      w.writeTree(newTree),
		Description: description,
		Author:      author,
		CommitTime:  time.Now().UTC(),
	}
	snap.RevisionHash = snapshotID(snap)

	// Atomic transaction: objects + snapshot + revision.
	tx, err := w.store.BeginTx()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Write the tree object and any blobs referenced by the tree (leaf blobs).
	// We re-walk the tree to collect all referenced blob/tree objects so they are
	// persisted. Blobs were created via WriteBlob (already persisted individually)
	// but to be safe we write the root tree object here.
	treeObj := &object.Object{Kind: object.KindTree, Tree: w.mustTree(snap.TreeID)}
	if err := w.store.WriteObjectsBatchTx(tx, []*object.Object{treeObj}); err != nil {
		return nil, nil, err
	}
	if err := w.store.PutSnapshotTx(tx, snap); err != nil {
		return nil, nil, err
	}
	rev := &store.Revision{
		ID: revisionID, Hash: snap.RevisionHash, Created: time.Now().UTC(),
		ChangedPaths: changedPaths,
	}
	if err := w.store.PutRevisionTx(tx, rev); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return snap, rev, nil
}
