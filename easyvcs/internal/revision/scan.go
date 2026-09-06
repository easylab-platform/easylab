package revision

import (
	"os"
	"path/filepath"

	"easyvcs/internal/ignore"
	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// scanResult is the read-only scan of a directory: the resulting in-memory tree
// plus a map from blob id to its content. No objects are written to the store.
type scanResult struct {
	tree    *object.Tree
	content map[object.ID][]byte
}

// scanTreeFromFS builds an in-memory Tree from a directory WITHOUT persisting
// any objects to the store. Blob content is kept in a side map so the caller
// can produce file changes. Ignore files are honored.
func scanTreeFromFS(dir string, m *ignore.Matcher) (*scanResult, error) {
	res := &scanResult{content: map[object.ID][]byte{}}
	var walk func(dir string, relPrefix string) (*object.Tree, error)
	walk = func(dir string, relPrefix string) (*object.Tree, error) {
		tree := object.NewTree()
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
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
					return nil, err
				}
				tree.Entries[name] = object.Entry{Name: name, Kind: object.KindTree, ID: sub.ID()}
				continue
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return nil, err
			}
			id := object.BlobID(data)
			res.content[id] = data
			tree.Entries[name] = object.Entry{Name: name, Kind: object.KindBlob, ID: id}
		}
		return tree, nil
	}
	root, err := walk(dir, "")
	if err != nil {
		return nil, err
	}
	res.tree = root
	return res, nil
}

// ComputeChangesFromDir diffs the parent snapshot's tree (if any) against a
// read-only scan of the working directory, returning a list of file changes.
// This is the "no workdir persistence" path: nothing is written to the store.
// If parentHash is empty, all files in the directory are reported as additions.
func (w *Workspace) ComputeChangesFromDir(parentHash object.ID, dir string) ([]FileChangeSpec, error) {
	var parentTree *object.Tree
	if parentHash != (object.ID{}) {
		psnap, err := w.store.GetSnapshot(parentHash)
		if err != nil {
			return nil, err
		}
		parentTree = w.mustTree(psnap.TreeID)
	} else {
		parentTree = object.NewTree()
	}
	m, _ := ignore.New(dir)
	work, err := scanTreeFromFS(dir, m)
	if err != nil {
		return nil, err
	}
	return diffTreesToSpecs(parentTree, work.tree, work.content, w), nil
}

// diffTreesToSpecs compares a stored parent tree with an in-memory scanned
// tree, producing FileChangeSpec for every path that differs. Content for
// added/modified paths is looked up in the scanned content map (the scanned
// blobs are not persisted).
func diffTreesToSpecs(a, b *object.Tree, scanned map[object.ID][]byte, w *Workspace) []FileChangeSpec {
	var out []FileChangeSpec
	w.diffSpecs("", a, b, scanned, &out)
	return out
}

func (w *Workspace) diffSpecs(prefix string, a, b *object.Tree, scanned map[object.ID][]byte, out *[]FileChangeSpec) {
	names := map[string]struct{}{}
	for _, t := range []*object.Tree{a, b} {
		for n := range t.Entries {
			names[n] = struct{}{}
		}
	}
	for name := range names {
		path := joinPath(prefix, name)
		ea, aHas := a.Entries[name]
		eb, bHas := b.Entries[name]
		subA, aIsTree := readTreeOrNil(ea, aHas, w)
		subB, bIsTree := readTreeOrNil(eb, bHas, w)
		switch {
		case aIsTree && bIsTree:
			w.diffSpecs(path, subA, subB, scanned, out)
		case aIsTree && !bIsTree:
			w.markDeleted(path, subA, out)
		case !aIsTree && bIsTree:
			w.markAdded(path, subB, scanned, out)
		default:
			aBlob := aHas && ea.Kind == object.KindBlob
			bBlob := bHas && eb.Kind == object.KindBlob
			switch {
			case aBlob && bBlob && ea.ID == eb.ID:
				// unchanged
			case aBlob && bBlob:
				if data, ok := scanned[eb.ID]; ok {
					*out = append(*out, FileChangeSpec{Path: path, Content: data})
				}
			case aBlob && !bBlob:
				*out = append(*out, FileChangeSpec{Path: path, Delete: true})
			case !aBlob && bBlob:
				if data, ok := scanned[eb.ID]; ok {
					*out = append(*out, FileChangeSpec{Path: path, Content: data})
				}
			}
		}
	}
}

func (w *Workspace) markDeleted(prefix string, t *object.Tree, out *[]FileChangeSpec) {
	for _, e := range t.SortedEntries() {
		path := joinPath(prefix, e.Name)
		if sub, ok := readTreeOrNil(e, true, w); ok {
			w.markDeleted(path, sub, out)
			continue
		}
		*out = append(*out, FileChangeSpec{Path: path, Delete: true})
	}
}

func (w *Workspace) markAdded(prefix string, t *object.Tree, scanned map[object.ID][]byte, out *[]FileChangeSpec) {
	for _, e := range t.SortedEntries() {
		path := joinPath(prefix, e.Name)
		if sub, ok := readTreeOrNil(e, true, w); ok {
			w.markAdded(path, sub, scanned, out)
			continue
		}
		if data, ok := scanned[e.ID]; ok {
			*out = append(*out, FileChangeSpec{Path: path, Content: data})
		}
	}
}

func readTreeOrNil(e object.Entry, present bool, w *Workspace) (*object.Tree, bool) {
	if !present || e.Kind != object.KindTree {
		return nil, false
	}
	t, err := w.ReadTree(e.ID)
	if err != nil {
		return nil, false
	}
	return t, true
}
