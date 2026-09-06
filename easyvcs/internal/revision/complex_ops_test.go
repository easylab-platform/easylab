package revision

import (
	"os"
	"path/filepath"
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// ---- helpers local to this suite ----

func treeWith(files map[string]string, ws *Workspace) *object.Tree {
	tree := object.NewTree()
	for name, content := range files {
		tree.Entries[name] = object.Entry{Name: name, Kind: object.KindBlob, ID: ws.WriteBlob([]byte(content))}
	}
	return tree
}

func namesOfTree(t *object.Tree) map[string]bool {
	out := map[string]bool{}
	for n := range t.Entries {
		out[n] = true
	}
	return out
}

func blobOf(ws *Workspace, tree *object.Tree, name string) string {
	e, ok := tree.Entries[name]
	if !ok {
		return ""
	}
	if e.Kind == object.KindConflict {
		return "<conflict>"
	}
	b, _ := ws.ReadBlob(e.ID)
	return string(b)
}

// commitChainFromTree commits each tree (with its explicit parent), returning
// snapshots and revisions keyed by name in order.
func commitChainFromTree(t *testing.T, ws *Workspace, ordered []string, trees map[string]*object.Tree) (map[string]*store.Snapshot, map[string]*store.Revision) {
	t.Helper()
	snaps := map[string]*store.Snapshot{}
	revs := map[string]*store.Revision{}
	var prev object.ID
	for _, name := range ordered {
		var parents []object.ID
		if prev != (object.ID{}) {
			parents = []object.ID{prev}
		}
		s, r := commitTreeAt(t, ws, trees[name], name, parents)
		snaps[name] = s
		revs[name] = r
		prev = s.RevisionHash
	}
	return snaps, revs
}

// --- Test 1: amend then rebase recomputes content from the amend's base ---

func TestAmendThenRebaseRecomputesContent(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// A: has x.txt = v1.
		ta := treeWith(map[string]string{"x.txt": "v1"}, ws)
		_, ra := commitTreeFromTree(t, ws, ta, "a")
		// Amend A: x.txt = v2 (same revision id).
		ta2 := treeWith(map[string]string{"x.txt": "v2"}, ws)
		s1, r1, amErr := ws.Commit(CommitParams{
			RevisionID:  ra.ID,
			TreeID:      ws.writeTree(ta2),
			Description: "a2",
			Author:      store.Author{Name: "t", Email: "t@x"},
		})
		if amErr != nil {
			t.Fatalf("amend: %v", amErr)
		}
		if r1.ID != ra.ID {
			t.Fatalf("amend must keep revision id: %s != %s", r1.ID, ra.ID)
		}
		// B: child of (amended) A, adds y.txt.
		tb := treeWith(map[string]string{"x.txt": "v2", "y.txt": "y"}, ws)
		sb, rb := commitTreeAt(t, ws, tb, "b", []object.ID{s1.RevisionHash})
		_ = rb

		// C: new side branch with a DIFFERENT x.txt, based on amended root.
		// Rebasing B onto C must recompute the merged x.txt as the 3-way result
		// of (base=a v2, ours=b(y added, x=v2), theirs=C). The id stays stable.
		tc := treeWith(map[string]string{"x.txt": "c", "z.txt": "z"}, ws)
		sc, rc := commitTreeFromTree(t, ws, tc, "c")
		_ = rc

		rebased, r2, err := ws.Rebase(rb.ID, []object.ID{sc.RevisionHash})
		if err != nil {
			t.Fatalf("rebase after amend: %v", err)
		}
		_ = r2
		if rebased.RevisionID != rb.ID {
			t.Fatalf("rebase must keep revision id: %s != %s", rebased.RevisionID, rb.ID)
		}
		// The rebased tree must contain BOTH the y.txt from B and z.txt from C.
		tr := ws.MustTree(rebased.TreeID)
		names := namesOfTree(tr)
		if !names["y.txt"] || !names["z.txt"] {
			t.Fatalf("rebased tree should merge y.txt (B) and z.txt (C): %v", names)
		}
		_ = sb
	})
}

// --- Test 2: rebase onto a base with an overlapping edit yields a conflict ---

func TestRebaseOverlapProducesConflict(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		base := treeWith(map[string]string{"f.txt": "base"}, ws)
		sbase, rbase := commitTreeFromTree(t, ws, base, "base")
		// Ours: f.txt = ours.
		ours := treeWith(map[string]string{"f.txt": "ours"}, ws)
		sours, _ := commitTreeAt(t, ws, ours, "ours", []object.ID{sbase.RevisionHash})
		// Theirs: f.txt = theirs.
		theirs := treeWith(map[string]string{"f.txt": "theirs"}, ws)
		stheirs, _ := commitTreeAt(t, ws, theirs, "theirs", []object.ID{sbase.RevisionHash})

		// Rebase ours onto theirs: f.txt diverges -> first-class conflict.
		reb, r3, err := ws.Rebase(sours.RevisionID, []object.ID{stheirs.RevisionHash})
		_ = r3
		if err != nil {
			t.Fatalf("rebase conflict: %v", err)
		}
		atoms, err := ws.ConflictsInTree(reb.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(atoms) != 1 || atoms[0].Path != "f.txt" {
			t.Fatalf("expected 1 conflict at f.txt, got %+v", atoms)
		}
		// Resolve to side 0 (ours); conflict gone and content = ours.
		rs, _, err := ws.Resolve(reb.RevisionID, "f.txt", 0)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		tr := ws.MustTree(rs.TreeID)
		if blobOf(ws, tr, "f.txt") != "ours" {
			t.Fatalf("resolved content should be 'ours', got %q", blobOf(ws, tr, "f.txt"))
		}
		_ = rbase
	})
}

// --- Test 3: dropping a middle revision of a longer chain keeps the rest ---

func TestDropMiddleOfLongerChain(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		ordered := []string{"a", "b", "c", "d", "e"}
		trees := map[string]*object.Tree{
			"a": treeWith(map[string]string{"a.txt": "a"}, ws),
			"b": treeWith(map[string]string{"a.txt": "a", "b.txt": "b"}, ws),
			"c": treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"}, ws),
			"d": treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c", "d.txt": "d"}, ws),
			"e": treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c", "d.txt": "d", "e.txt": "e"}, ws),
		}
		snaps, revs := commitChainFromTree(t, ws, ordered, trees)

		// Drop "c": descendants d,e must rebase onto b, producing a-b-d-e.
		moved, err := ws.Drop(revs["c"].ID)
		if err != nil {
			t.Fatalf("drop c: %v", err)
		}
		if len(moved) != 2 {
			t.Fatalf("expected d,e moved, got %d", len(moved))
		}
		// d rebases onto b; its tree has a,b,d (no c).
		dSnap, _ := ws.GetSnapshot(moved[0].Hash)
		if len(dSnap.Parents) != 1 || dSnap.Parents[0] != snaps["b"].RevisionHash {
			t.Fatalf("d should parent=b, got %v", dSnap.Parents)
		}
		dt := ws.MustTree(dSnap.TreeID)
		names := namesOfTree(dt)
		if names["c.txt"] {
			t.Fatalf("d should not retain c.txt after drop: %v", names)
		}
		if !names["d.txt"] {
			t.Fatalf("d should keep d.txt: %v", names)
		}
		// e chains onto new d, and its tree has a,b,d,e (no c).
		eSnap, _ := ws.GetSnapshot(moved[1].Hash)
		if len(eSnap.Parents) != 1 || eSnap.Parents[0] != moved[0].Hash {
			t.Fatalf("e should parent=new d, got %v", eSnap.Parents)
		}
		et := ws.MustTree(eSnap.TreeID)
		if namesOfTree(et)["c.txt"] {
			t.Fatalf("e should not retain c.txt: %v", namesOfTree(et))
		}
		// c snapshot now unreferenced.
		referenced := map[string]bool{}
		revs2, _ := ws.Log()
		for _, r := range revs2 {
			s, _ := ws.GetSnapshot(r.Hash)
			for _, p := range s.Parents {
				referenced[p.String()] = true
			}
		}
		if referenced[snaps["c"].RevisionHash.String()] {
			t.Fatal("c should be unreachable after drop")
		}
	})
}

// --- Test 4: two gitignore files merge as ordinary blobs, with a real ignore case ---

func TestDifferentGitignoreNodesMerge(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Build A from a real FS whose .gitignore excludes *.log.
		buildFS(t, dir, ".gitignore", "*.log\n")
		buildFS(t, dir, "a.txt", "a\n")
		buildFS(t, dir, "junk.log", "junk\n") // should be excluded
		sa, _ := commitTree(t, ws, dir, "a", nil)
		if treeHas(ws, sa.TreeID, "junk.log") {
			t.Fatal("ignored junk.log should not be tracked in A")
		}

		// Build B on its own root with a different .gitignore (ignores tmp/).
		dir2 := t.TempDir()
		buildFS(t, dir2, ".gitignore", "tmp/\n")
		buildFS(t, dir2, "b.txt", "b\n")
		buildFS(t, dir2, "tmp/x.txt", "x\n") // ignored
		sb, rb := commitTree(t, ws, dir2, "b", nil)
		if treeHas(ws, sb.TreeID, "tmp") {
			t.Fatal("ignored tmp/ should not be tracked in B")
		}

		// Rebase B onto A: two distinct .gitignore blobs merge. Because both A
		// and B ADD a .gitignore (path absent in the shared base), the 3-way
		// merge produces a first-class conflict at .gitignore — this is correct
		// and matches git/jj semantics.
		reb, r3, err := ws.Rebase(rb.ID, []object.ID{sa.RevisionHash})
		_ = r3
		if err != nil {
			t.Fatalf("rebase different-ignore node: %v", err)
		}
		// The .gitignore path is a conflict object.
		atoms, err := ws.ConflictsInTree(reb.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(atoms) != 1 || atoms[0].Path != ".gitignore" {
			t.Fatalf("expected a conflict at .gitignore, got %+v", atoms)
		}
		tr := ws.MustTree(reb.TreeID)
		if gi := blobOf(ws, tr, ".gitignore"); gi != "<conflict>" {
			t.Fatalf("gitignore should be a conflict object, got %q", gi)
		}
		// Resolve to the "ours" side (B's rule) and confirm coherence.
		rs, _, err := ws.Resolve(reb.RevisionID, ".gitignore", 0)
		if err != nil {
			t.Fatalf("resolve gitignore: %v", err)
		}
		rt := ws.MustTree(rs.TreeID)
		if got := blobOf(ws, rt, ".gitignore"); got != "tmp/\n" {
			t.Fatalf("resolved gitignore should be B's rule, got %q", got)
		}
		// The merged tree still contains A's a.txt and B's b.txt.
		names := namesOfTree(rt)
		if !names["a.txt"] || !names["b.txt"] {
			t.Fatalf("merged tree missing both sides' files: %v", names)
		}
	})
}

func treeHas(ws *Workspace, treeID object.ID, name string) bool {
	tr := ws.MustTree(treeID)
	return namesOfTree(tr)[name]
}

// --- Test 5: rebase a ROOT revision onto another root ---

func TestRebaseRootOntoBase(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// A: root with x.txt.
		ta := treeWith(map[string]string{"x.txt": "x"}, ws)
		_, ra := commitTreeFromTree(t, ws, ta, "a")
		// B: separate root with y.txt.
		tb := treeWith(map[string]string{"y.txt": "y"}, ws)
		_, rb := commitTreeFromTree(t, ws, tb, "b")

		// Rebase A (a root) onto B: base = empty, ours = A (x.txt), theirs = B.
		reb, r3, err := ws.Rebase(ra.ID, []object.ID{rb.Hash})
		_ = r3
		if err != nil {
			t.Fatalf("rebase root onto base: %v", err)
		}
		if reb.RevisionID != ra.ID {
			t.Fatalf("root rebase must keep id: %s != %s", reb.RevisionID, ra.ID)
		}
		// Result tree has both A's x.txt and B's y.txt.
		tr := ws.MustTree(reb.TreeID)
		names := namesOfTree(tr)
		if !names["x.txt"] || !names["y.txt"] {
			t.Fatalf("root-rebase should combine both trees: %v", names)
		}
	})
}

// --- Test 6: revert then rebase interplay (history preserved), then drop ---

func TestRevertThenRebaseInterplay(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		ordered := []string{"a", "b", "c"}
		trees := map[string]*object.Tree{
			"a": treeWith(map[string]string{"a.txt": "a"}, ws),
			"b": treeWith(map[string]string{"a.txt": "a", "b.txt": "b"}, ws),
			"c": treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"}, ws),
		}
		snaps, revs := commitChainFromTree(t, ws, ordered, trees)

		// Revert b: undoes b.txt onto the tip (c).
		rev, rv2, err := ws.Revert(revs["b"].ID, object.ID{})
		_ = rv2
		if err != nil {
			t.Fatalf("revert b: %v", err)
		}
		rt := ws.MustTree(rev.TreeID)
		if namesOfTree(rt)["b.txt"] {
			t.Fatalf("revert should remove b.txt from tree: %v", namesOfTree(rt))
		}
		// b still exists as a revision (history preserved).
		if _, err := ws.GetRevision(revs["b"].ID); err != nil {
			t.Fatalf("b should remain after revert: %v", err)
		}

		// Now a new branch D on top of c, and we rebase D onto the revert rev.
		td := treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c", "d.txt": "d"}, ws)
		sd, rd := commitTreeAt(t, ws, td, "d", []object.ID{snaps["c"].RevisionHash})
		_ = sd

		reb, r3, err := ws.Rebase(rd.ID, []object.ID{rev.RevisionHash})
		_ = r3
		if err != nil {
			t.Fatalf("rebase d onto revert rev: %v", err)
		}
		// The rebased d must NOT have b.txt (carried over from reverted b-side)
		// but MUST keep c.txt and d.txt.
		tr := ws.MustTree(reb.TreeID)
		names := namesOfTree(tr)
		if names["b.txt"] {
			t.Fatalf("rebase onto reverting rev should drop b.txt: %v", names)
		}
		if !names["c.txt"] || !names["d.txt"] {
			t.Fatalf("rebase should keep c.txt,d.txt: %v", names)
		}
	})
}

// --- Test 7: squash then rebase keeps ids linear ---

func TestSquashThenRebaseKeepsLinear(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		ordered := []string{"a", "b", "c"}
		trees := map[string]*object.Tree{
			"a": treeWith(map[string]string{"a.txt": "a"}, ws),
			"b": treeWith(map[string]string{"a.txt": "a", "b.txt": "b"}, ws),
			"c": treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"}, ws),
		}
		snaps, revs := commitChainFromTree(t, ws, ordered, trees)

		// Squash c into b -> a single new revision over a.
		sq, sq2, err := ws.Squash(revs["c"].ID)
		_ = sq2
		if err != nil {
			t.Fatalf("squash c: %v", err)
		}
		// Rebase the squashed revision onto a fresh root E. The squashed diff is
		// "add b,c" (a is context from the old base). Rebasing onto E re-applies
		// that diff, so the result is e + b + c — a.txt (pure old-base context)
		// correctly falls away, mirroring git rebase onto a new base.
		te := treeWith(map[string]string{"e.txt": "e"}, ws)
		se, re := commitTreeFromTree(t, ws, te, "e")
		reb, r3, err := ws.Rebase(sq.RevisionID, []object.ID{re.Hash})
		_ = r3
		if err != nil {
			t.Fatalf("rebase squashed rev: %v", err)
		}
		if reb.RevisionID != sq.RevisionID {
			t.Fatalf("rebase of squashed should keep id: %s != %s", reb.RevisionID, sq.RevisionID)
		}
		tr := ws.MustTree(reb.TreeID)
		names := namesOfTree(tr)
		for _, want := range []string{"e.txt", "b.txt", "c.txt"} {
			if !names[want] {
				t.Fatalf("squashed+rebased tree missing %s: %v", want, names)
			}
		}
		if names["a.txt"] {
			t.Fatalf("old-base context a.txt should be dropped: %v", names)
		}
		_ = snaps
		_ = se
	})
}

// --- Test 8: RebaseMany skips a whole middle sub-chain correctly ---

func TestRebaseManyJumpsOverChain(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		ordered := []string{"a", "b", "c", "d", "e"}
		trees := map[string]*object.Tree{
			"a": treeWith(map[string]string{"a.txt": "a"}, ws),
			"b": treeWith(map[string]string{"a.txt": "a", "b.txt": "b"}, ws),
			"c": treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"}, ws),
			"d": treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c", "d.txt": "d"}, ws),
			"e": treeWith(map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c", "d.txt": "d", "e.txt": "e"}, ws),
		}
		snaps, revs := commitChainFromTree(t, ws, ordered, trees)

		// Move only [d, e] onto a: skips b,c entirely -> a-d-e.
		moved, err := ws.RebaseMany([]string{revs["d"].ID, revs["e"].ID}, snaps["a"].RevisionHash)
		if err != nil {
			t.Fatalf("RebaseMany jump: %v", err)
		}
		if len(moved) != 2 {
			t.Fatalf("expected 2 moved, got %d", len(moved))
		}
		// d onto a: tree has a.txt + d.txt (no b,c).
		dt := ws.MustTree(moved[0].Hash)
		if namesOfTree(dt)["b.txt"] || namesOfTree(dt)["c.txt"] {
			t.Fatalf("d jumped over b,c: %v", namesOfTree(dt))
		}
		// e chains onto d.
		et := ws.MustTree(moved[1].Hash)
		if namesOfTree(et)["b.txt"] || namesOfTree(et)["c.txt"] {
			t.Fatalf("e jumped over b,c: %v", namesOfTree(et))
		}
	})
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ = filepath.Join
var _ = os.Getenv
