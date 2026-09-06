// Package merge implements N-way tree merge and first-class conflicts.
//
// A merge combines two or more divergent trees relative to a common base. When
// the changes don't conflict, the result is a merged tree. When they do,
// EasyVCS records a conflict as a first-class, content-addressed object (like
// jj's Merge<T>) rather than failing the operation. The conflict object is
// stored in the tree at the conflicting path and can be surfaced, resolved, or
// propagated to descendants.
package merge

import (
	"fmt"
	"sort"

	"easyvcs/internal/object"
)

// treeOps is the minimal object access needed by a merge. It is implemented by
// change.Workspace, keeping merge independent of the storage backend.
type treeOps interface {
	ReadBlob(id object.ID) ([]byte, error)
	ReadTree(id object.ID) (*object.Tree, error)
	WriteBlob(data []byte) object.ID
	WriteTree(t *object.Tree) object.ID
	WriteConflict(c *object.Conflict) object.ID
}

// ConflictAtom is a lightweight record of a conflict location in a merged
// tree: the path and the conflict object id stored there.
type ConflictAtom struct {
	Path string
	ID   object.ID
}

// absent is a sentinel id representing "no content / deleted".
var absent object.ID

// mergeTerms is the working set of participating entries for one path.
type mergeTerms struct {
	// hasBase indicates whether a common ancestor existed at this path.
	hasBase bool
	base    object.ID
	// ours/theirs presence flags.
	hasOurs, hasTheirs bool
	ours, theirs       object.ID
}

// Trees performs a recursive 3-way tree merge. It returns the merged tree
// (with conflict objects embedded at conflicting paths) and the list of
// conflict atoms that were produced.
func Trees(base, ours, theirs *object.Tree, ops treeOps) (*object.Tree, []ConflictAtom) {
	merged := object.NewTree()
	var atoms []ConflictAtom

	names := map[string]struct{}{}
	for _, t := range []*object.Tree{base, ours, theirs} {
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
		b := base.Entries[name]
		o := ours.Entries[name]
		t := theirs.Entries[name]

		entry, subAtoms := mergeEntry(name, b, o, t, ops)
		atoms = append(atoms, subAtoms...)
		if entry != nil {
			merged.Entries[name] = *entry
		}
	}
	return merged, atoms
}

func blankID() object.ID { var z object.ID; return z }

// present reports whether an entry actually holds content, i.e. its blob id is
// non-zero. A missing (zero-value) Entry has Kind==KindBlob and ID==0, which
// otherwise would be mistaken for a present empty blob. We treat a non-zero id
// as present, and the zero id as "absent" (deleted/not added).
func present(e object.Entry) bool { return e.ID != (object.ID{}) }

func isTree(e object.Entry) bool { return e.Kind == object.KindTree }

func mergeEntry(name string, base, ours, theirs object.Entry, ops treeOps) (*object.Entry, []ConflictAtom) {
	baseTree, baseIsTree := entryAsTree(base, ops)
	oursTree, oursIsTree := entryAsTree(ours, ops)
	theirsTree, theirsIsTree := entryAsTree(theirs, ops)

	// If any side is a tree, merge as trees (missing sides are empty subtrees).
	if baseIsTree || oursIsTree || theirsIsTree {
		bt, ot, tt := baseTree, oursTree, theirsTree
		if bt == nil {
			bt = object.NewTree()
		}
		if ot == nil {
			ot = object.NewTree()
		}
		if tt == nil {
			tt = object.NewTree()
		}
		sub, subAtoms := Trees(bt, ot, tt, ops)
		for i := range subAtoms {
			subAtoms[i].Path = joinPath(name, subAtoms[i].Path)
		}
		if len(sub.Entries) == 0 {
			return nil, subAtoms
		}
		return &object.Entry{Name: name, Kind: object.KindTree, ID: ops.WriteTree(sub)}, subAtoms
	}

	// Determine presence of each side as a blob (or absent), by non-zero id.
	baseP := present(base)
	oursP := present(ours)
	theirsP := present(theirs)

	// Values of the three sides (absent -> zero id).
	bv, ov, tv := blankID(), blankID(), blankID()
	if baseP {
		bv = base.ID
	}
	if oursP {
		ov = ours.ID
	}
	if theirsP {
		tv = theirs.ID
	}

	// Resolve automatically where possible.
	// Both sides identical.
	if oursP && theirsP && ours.ID == theirs.ID {
		return &ours, nil
	}
	// One side unchanged from base.
	if baseP && oursP && theirsP {
		if ours.ID == base.ID {
			return &theirs, nil
		}
		if theirs.ID == base.ID {
			return &ours, nil
		}
	}
	// One side unchanged from base, the other deleted -> take the deletion
	// (a non-conflicting 3-way). This is what makes rebase/drop/revert
	// re-apply a diff correctly:
	//   - a path that is IN the base and unchanged by the rebased side (ours)
	//     but absent on the new base (theirs) is dropped;
	//   - a path that a reverted side (ours) removed while the onto tip
	//     (theirs) still holds the base version is dropped too.
	if baseP {
		if oursP && !theirsP && ours.ID == base.ID {
			return nil, nil
		}
		if theirsP && !oursP && theirs.ID == base.ID {
			return nil, nil
		}
	}
	// Added only on one side.
	if oursP && !theirsP && !baseP {
		return &ours, nil
	}
	if !oursP && theirsP && !baseP {
		return &theirs, nil
	}
	// Deleted on both sides.
	if !oursP && !theirsP && baseP {
		return nil, nil
	}
	// Otherwise this is a genuine conflict (N-way). Build a conflict object
	// whose removes = base (if any) and adds = the present ours/theirs terms
	// (if present). Deletion is represented by an absent term.
	c := &object.Conflict{Removes: []object.Term{}, Adds: []object.Term{}}
	if baseP {
		c.Removes = append(c.Removes, object.Term{ID: bv, Label: "base"})
	}
	if oursP {
		c.Adds = append(c.Adds, object.Term{ID: ov, Label: "ours"})
	}
	if theirsP {
		c.Adds = append(c.Adds, object.Term{ID: tv, Label: "theirs"})
	}
	conflictID := ops.WriteConflict(c)
	// Absent side needs an explicit term so resolve can produce a deletion. If a
	// side is absent, add an absent (zero-id) positive term with that label.
	if !oursP && !theirsP {
		c.Adds = append(c.Adds, object.Term{ID: absent, Label: "theirs"})
	}
	if !oursP && theirsP {
		c.Adds = append(c.Adds, object.Term{ID: absent, Label: "ours"})
	}
	if !theirsP && oursP {
		c.Adds = append(c.Adds, object.Term{ID: absent, Label: "theirs"})
	}
	// Recompute id after adding absent-side terms.
	conflictID = ops.WriteConflict(c)
	return &object.Entry{Name: name, Kind: object.KindConflict, ID: conflictID}, []ConflictAtom{{Path: name, ID: conflictID}}
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return fmt.Sprintf("%s/%s", parent, child)
}

func entryAsTree(e object.Entry, ops treeOps) (*object.Tree, bool) {
	if e.Kind != object.KindTree {
		return nil, false
	}
	t, err := ops.ReadTree(e.ID)
	if err != nil {
		return nil, false
	}
	return t, true
}
