// Package revision implements the semantic layer of EasyVCS.
//
// It is the only package that depends on the store.RepoStore interface for
// persistence; it encodes the two rules that make EasyVCS change-native:
//
//  1. A Revision has a stable id (revision_id) that is generated once and never
//     modified, no matter how many times the revision is rebased, squashed, or
//     amended. It is the user-facing unit of work.
//  2. A Revision points at exactly one current Snapshot (1:1). Each rewrite
//     (amend/rebase/squash/resolve) produces a new Snapshot with a new
//     revision_hash, which replaces the previous one; the revision_id stays
//     stable forever.
package revision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"easyvcs/internal/ignore"
	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// ErrRevisionNotFound is returned when a revision id has no record.
var ErrRevisionNotFound = errors.New("revision: not found")

// ErrChangeNotFound is an alias kept for compatibility.
var ErrChangeNotFound = ErrRevisionNotFound

// Workspace is the semantic API over a repo-scoped store.
type Workspace struct {
	store store.RepoStore
}

// NewWorkspace wraps a repo-scoped store in the semantic layer.
func NewWorkspace(s store.RepoStore) *Workspace { return &Workspace{store: s} }

// BuildTreeFromFS reads a filesystem directory and writes the corresponding
// Tree object plus all reachable blob objects into the store. It returns the
// root tree id.
func (w *Workspace) BuildTreeFromFS(dir string) (object.ID, error) {
	return w.buildTreeFromFS(dir)
}

// Path is a repo-relative path with forward slashes.
type Path = string

// buildTreeFromFS recursively walks a directory, writing blob and tree objects
// to the store, and returns the root tree id. Entries matching .gitignore /
// .vcsignore patterns (relative to the walk root) are excluded. The .easyvcs
// metadata dir and the workspace marker file are always skipped.
func (w *Workspace) buildTreeFromFS(dir string) (object.ID, error) {
	m, _ := ignore.New(dir)
	return w.buildTreeFromFSFiltered(dir, m)
}

func (w *Workspace) buildTreeFromFSFiltered(root string, m *ignore.Matcher) (object.ID, error) {
	var walk func(dir string, relPrefix string) (object.ID, error)
	walk = func(dir string, relPrefix string) (object.ID, error) {
		tree := object.NewTree()
		entries, err := os.ReadDir(dir)
		if err != nil {
			return object.ID{}, err
		}
		for _, de := range entries {
			name := de.Name()
			rel := joinPath(relPrefix, name)
			if name == ".easyvcs" || name == ".git" || name == store.WorkspaceMarkerFile {
				continue
			}
			if m != nil && m.Ignored(rel) {
				continue
			}
			full := filepath.Join(dir, name)
			if de.IsDir() {
				sub, err := walk(full, rel)
				if err != nil {
					return object.ID{}, err
				}
				// Prune empty directories (e.g. a dir that only contains
				// ignored children) so a directory-only ignore rule has no
				// residue in the tree.
				subTree := w.mustTree(sub)
				if len(subTree.Entries) == 0 {
					continue
				}
				tree.Entries[name] = object.Entry{Name: name, Kind: object.KindTree, ID: sub}
				continue
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return object.ID{}, err
			}
			blob := &object.Object{Kind: object.KindBlob, Blob: data}
			if err := w.store.WriteObject(blob); err != nil {
				return object.ID{}, err
			}
			tree.Entries[name] = object.Entry{Name: name, Kind: object.KindBlob, ID: blob.ID()}
		}
		return w.writeTree(tree), nil
	}
	return walk(root, "")
}

// BuildTreeFromFSWithMatcher builds a tree using an explicit matcher (exposed
// for tests and for external ignore.capture).
func (w *Workspace) BuildTreeFromFSWithMatcher(root string, m *ignore.Matcher) (object.ID, error) {
	return w.buildTreeFromFSFiltered(root, m)
}

// IsIgnored reports whether a repo-relative path is ignored by the matcher
// compiled from the given root.
func IsIgnored(m *ignore.Matcher, rel string) bool { return m != nil && m.Ignored(rel) }

// CommitParams carries the inputs to a commit.
type CommitParams struct {
	// RevisionID, if non-empty, commits to an existing revision (amending it).
	// If empty, a new revision id is generated.
	RevisionID string
	// Parents are the parent snapshot ids (typically from a previous snapshot).
	Parents []object.ID
	// Description is the commit message.
	Description string
	// Author of the commit.
	Author store.Author
	// TreeID is the root tree for the commit.
	TreeID object.ID
}

// Commit persists a new snapshot (with a new revision_hash) under a revision
// (existing or new), and returns the snapshot and the revision. Because
// Revision and Snapshot are 1:1, committing always creates one snapshot and
// points the revision's Hash at it, replacing any previous snapshot.
//
// If CommitParams.RevisionID is empty, a new revision is created and its parent
// is the given Parents (used for linear history). If RevisionID is non-empty,
// the existing revision is amended and its ChangedPaths are updated to the
// paths that differ from the previous snapshot (if any) or the given Parents.
func (w *Workspace) Commit(p CommitParams) (*store.Snapshot, *store.Revision, error) {
	revisionID := p.RevisionID
	if revisionID == "" {
		var err error
		revisionID, err = object.RandomChangeID()
		if err != nil {
			return nil, nil, err
		}
	}
	snap := &store.Snapshot{
		RevisionID:  revisionID,
		Parents:     p.Parents,
		TreeID:      p.TreeID,
		Description: p.Description,
		Author:      p.Author,
		CommitTime:  time.Now().UTC(),
	}
	snap.RevisionHash = snapshotID(snap)

	if err := w.store.PutSnapshot(snap); err != nil {
		return nil, nil, err
	}

	var rev *store.Revision
	var err error
	if revisionID != "" {
		rev, err = w.store.GetRevision(revisionID)
	}
	if err != nil || rev == nil {
		rev = &store.Revision{ID: revisionID, Hash: snap.RevisionHash, Created: time.Now().UTC()}
	} else {
		rev.Hash = snap.RevisionHash
	}
	// Compute changed paths relative to the parent snapshot(s). For a new
	// revision, diff against the first parent's tree (or the root). For an
	// amend, diff against the *previous* snapshot tree.
	rev.ChangedPaths = w.changedPathsBetween(p.Parents, snap.RevisionHash)
	if err := w.store.PutRevision(rev); err != nil {
		return nil, nil, err
	}
	return snap, rev, nil
}

// changedPathsBetween returns the repo-relative paths that differ between the
// snapshot(s) referenced by parents and the current snapshot hash. Empty if
// there is no parent (root revision), all new paths are reported.
func (w *Workspace) changedPathsBetween(parents []object.ID, cur object.ID) []string {
	var prevTree object.ID
	if len(parents) > 0 {
		if psnap, err := w.store.GetSnapshot(parents[0]); err == nil {
			prevTree = psnap.TreeID
		}
	}
	curSnap, err := w.store.GetSnapshot(cur)
	if err != nil {
		return nil
	}
	if prevTree == (object.ID{}) {
		// Root revision: all files are "added". Gather every path.
		return w.allTreePaths(curSnap.TreeID)
	}
	changes, err := w.Diff(prevTree, curSnap.TreeID)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	return paths
}

// allTreePaths lists every file path in a tree (used for root revisions).
func (w *Workspace) allTreePaths(treeID object.ID) []string {
	var out []string
	w.collectPaths("", w.mustTree(treeID), &out)
	return out
}

func (w *Workspace) collectPaths(prefix string, tree *object.Tree, out *[]string) {
	for _, e := range tree.SortedEntries() {
		path := joinPath(prefix, e.Name)
		if e.Kind == object.KindTree {
			sub, err := w.ReadTree(e.ID)
			if err != nil {
				continue
			}
			w.collectPaths(path, sub, out)
			continue
		}
		*out = append(*out, path)
	}
}

// SnapshotID exposes the content-addressed hash computation for a snapshot.
func SnapshotID(s *store.Snapshot) object.ID { return snapshotID(s) }

// CollectPaths lists every file path in a tree (exported).
func (w *Workspace) CollectPaths(treeID object.ID) ([]string, error) {
	var out []string
	w.collectPaths("", w.mustTree(treeID), &out)
	return out, nil
}

func snapshotID(s *store.Snapshot) object.ID {
	payload := &store.Snapshot{}
	*payload = *s
	var b strings.Builder
	fmt.Fprintf(&b, "revision=%s\n", s.RevisionID)
	fmt.Fprintf(&b, "tree=%s\n", s.TreeID)
	fmt.Fprintf(&b, "description=%s\n", s.Description)
	fmt.Fprintf(&b, "author=%s\n", s.Author)
	for _, p := range s.Parents {
		fmt.Fprintf(&b, "parent=%s\n", p)
	}
	return object.BlobID([]byte(b.String()))
}

// GetRevision returns a revision by id.
func (w *Workspace) GetRevision(id string) (*store.Revision, error) {
	c, err := w.store.GetRevision(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrRevisionNotFound
	}
	return c, err
}

// GetChange is an alias of GetRevision kept for compatibility.
func (w *Workspace) GetChange(id string) (*store.Revision, error) {
	return w.GetRevision(id)
}

// ParentsOfRevision returns the parent snapshot ids of a revision's current
// snapshot. If the revision or snapshot is missing, it returns an empty slice.
func (w *Workspace) ParentsOfRevision(revisionID string) ([]object.ID, error) {
	if revisionID == "" {
		return nil, nil
	}
	rev, err := w.store.GetRevision(revisionID)
	if err != nil {
		return nil, nil
	}
	snap, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, nil
	}
	return snap.Parents, nil
}

// ParentsOfChange is an alias of ParentsOfRevision kept for compatibility.
func (w *Workspace) ParentsOfChange(changeID string) ([]object.ID, error) {
	return w.ParentsOfRevision(changeID)
}

// GetSnapshot returns a snapshot by id.
func (w *Workspace) GetSnapshot(id object.ID) (*store.Snapshot, error) {
	return w.store.GetSnapshot(id)
}

// ReadTree reads a tree object from the store.
func (w *Workspace) ReadTree(id object.ID) (*object.Tree, error) {
	o, err := w.store.ReadObject(id)
	if err != nil {
		return nil, err
	}
	if o.Kind != object.KindTree || o.Tree == nil {
		return nil, fmt.Errorf("object %s is not a tree", id)
	}
	return o.Tree, nil
}

// Log returns the revisions sorted by most-recent first.
func (w *Workspace) Log() ([]*store.Revision, error) {
	return w.store.ListRevisions()
}

// AllSnapshots returns every snapshot reachable as a current snapshot of some
// revision, associated with its revision id, for graph display.
func (w *Workspace) AllSnapshots() (map[string]*store.Snapshot, map[object.ID]string, error) {
	revisions, err := w.store.ListRevisions()
	if err != nil {
		return nil, nil, err
	}
	byRevision := map[string]*store.Snapshot{}
	byID := map[object.ID]string{}
	for _, rv := range revisions {
		snap, err := w.store.GetSnapshot(rv.Hash)
		if err != nil {
			continue
		}
		byRevision[rv.ID] = snap
		byID[snap.RevisionHash] = rv.ID
	}
	return byRevision, byID, nil
}

// ResolveRevisions resolves user input expressions to snapshot ids via the
// current revision pointers. Supported forms are '@', '@-', and a revision id.
func (w *Workspace) ResolveRevisions(exprs []string) ([]object.ID, []*store.Revision, error) {
	revisions, err := w.store.ListRevisions()
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(revisions, func(i, j int) bool {
		return revisions[i].Created.After(revisions[j].Created)
	})
	var ids []object.ID
	var rvs []*store.Revision
	for _, e := range exprs {
		if e == "@" {
			if len(revisions) > 0 {
				ids = append(ids, revisions[0].Hash)
				rvs = append(rvs, revisions[0])
			}
			continue
		}
		if e == "@-" {
			if len(revisions) > 0 {
				snap, err2 := w.store.GetSnapshot(revisions[0].Hash)
				if err2 == nil && len(snap.Parents) > 0 {
					ids = append(ids, snap.Parents[0])
				}
			}
			continue
		}
		found := findRevision(revisions, e)
		if found != nil {
			ids = append(ids, found.Hash)
			rvs = append(rvs, found)
		}
	}
	return ids, rvs, nil
}

func findRevision(revisions []*store.Revision, id string) *store.Revision {
	for _, rv := range revisions {
		if rv.ID == id || strings.HasPrefix(rv.ID, id) {
			return rv
		}
	}
	return nil
}

// findChange is an alias of findRevision kept for compatibility.
func findChange(revisions []*store.Revision, id string) *store.Revision {
	return findRevision(revisions, id)
}
