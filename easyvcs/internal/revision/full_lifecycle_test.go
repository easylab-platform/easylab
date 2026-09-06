package revision

import (
	"testing"
	"time"

	"easyvcs/internal/ignore"
	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

func mustRandomID(t *testing.T) string {
	t.Helper()
	id, err := object.RandomChangeID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nowUtc() time.Time { return time.Now().UTC() }

// storePut persists a snapshot and revision for a raw test object.
func (w *Workspace) storePut(snap *store.Snapshot, rev *store.Revision) error {
	if err := w.store.PutSnapshot(snap); err != nil {
		return err
	}
	return w.store.PutRevision(rev)
}

// TestFullRepoLifecycle is a single serial integration test that drives one
// repository through its full lifecycle: commit chain, amends, content changes,
// rebases, squashes, conflicts + resolve, branchs/tags, refs, file history,
// diff, ignore rewriting, merge, and queries. It asserts invariants at each step
// to ensure change-native semantics hold end-to-end.
//
// It runs against both the file-backed and SQLite backends via runBoth.
func TestFullRepoLifecycle(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// ---- Phase 1: linear commit chain ----
		buildFS(t, dir, "src/main.go", "package main\n")
		buildFS(t, dir, "README.md", "hello\n")
		s1, r1 := commitTree(t, ws, dir, "initial import", nil)
		if r1.ID == "" || s1.RevisionHash == (object.ID{}) {
			t.Fatal("first revision empty")
		}

		buildFS(t, dir, "src/main.go", "package main\n\nfunc main(){}\n")
		buildFS(t, dir, "src/util.go", "package main\n")
		s2, _ := commitTree(t, ws, dir, "add util", []object.ID{s1.RevisionHash})

		buildFS(t, dir, "docs/guide.md", "guide\n")
		s3, r3 := commitTree(t, ws, dir, "docs", []object.ID{s2.RevisionHash})

		// Linear chain parent linkage.
		if len(s2.Parents) != 1 || s2.Parents[0] != s1.RevisionHash {
			t.Fatalf("s2 parent: %v", s2.Parents)
		}
		if len(s3.Parents) != 1 || s3.Parents[0] != s2.RevisionHash {
			t.Fatalf("s3 parent: %v", s3.Parents)
		}

		// ---- Phase 2: file histories ----
		edits, err := ws.FileHistory("", "src/main.go")
		if err != nil {
			t.Fatal(err)
		}
		// main.go changed in r1 (added) and r2 (modified) => 2 edits.
		if len(edits) != 2 {
			t.Fatalf("main.go history: %d", len(edits))
		}
		// docs/guide.md only in r3 => 1 edit.
		editsGuide, _ := ws.FileHistory("", "docs/guide.md")
		if len(editsGuide) != 1 {
			t.Fatalf("guide history: %d", len(editsGuide))
		}
		// README only added in r1 => 1 edit.
		editsReadme, _ := ws.FileHistory("", "README.md")
		if len(editsReadme) != 1 {
			t.Fatalf("readme history: %d", len(editsReadme))
		}

		// ---- Phase 3: revision queries (ResolveRevisions) ----
		ids, rvs, err := ws.ResolveRevisions([]string{"@"})
		if err != nil || len(ids) != 1 || ids[0] != s3.RevisionHash {
			t.Fatalf("resolve @: %v %v", ids, err)
		}
		_ = rvs
		ids1, _, _ := ws.ResolveRevisions([]string{r1.ID[:6]})
		if len(ids1) != 1 || ids1[0] != s1.RevisionHash {
			t.Fatalf("resolve r1: %v", ids1)
		}
		// @- (parent of newest)
		idsAtMinus, _, _ := ws.ResolveRevisions([]string{"@-"})
		if len(idsAtMinus) != 1 || idsAtMinus[0] != s2.RevisionHash {
			t.Fatalf("resolve @-: %v", idsAtMinus)
		}
		// AllSnapshots mapping.
		byRev, byID, _ := ws.AllSnapshots()
		if len(byRev) != 3 || len(byID) != 3 {
			t.Fatalf("all snapshots: %d %d", len(byRev), len(byID))
		}
		if byID[s3.RevisionHash] != r3.ID {
			t.Fatal("byID mapping wrong")
		}

		// ---- Phase 4: branch/tag/ref ----
		if _, err := ws.SetRef("main", store.RefBranch, r3.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.SetRef("v1", store.RefTag, r1.ID); err != nil {
			t.Fatal(err)
		}
		refs, _ := ws.ListRefs()
		if len(refs) != 2 {
			t.Fatalf("refs: %d", len(refs))
		}
		gotMain, _ := ws.GetRef("main")
		if gotMain.Target != r3.ID || gotMain.Kind != store.RefBranch {
			t.Fatalf("main ref: %+v", gotMain)
		}
		if err := ws.DeleteRef("v1"); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.GetRef("v1"); err == nil {
			t.Fatal("v1 should be gone")
		}

		// ---- Phase 5: diff ----
		diff, err := ws.Diff(s1.TreeID, s2.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		// r2 changed src/main.go + added src/util.go.
		if len(diff) != 2 {
			t.Fatalf("diff r1->r2: %d", len(diff))
		}
		contentDiff, err := ws.DiffContent(s1.TreeID, s2.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(contentDiff) != 2 {
			t.Fatalf("content diff r1->r2: %d", len(contentDiff))
		}

		// ---- Phase 6: rebase (reparent a revision) ----
		// Rebase r3's snapshot onto r1 (reparent docs onto initial).
		rebasedSnap, rebasedRev, err := ws.Rebase(r3.ID, []object.ID{s1.RevisionHash})
		if err != nil {
			t.Fatal(err)
		}
		if rebasedRev.ID != r3.ID {
			t.Fatalf("rebase changed revision id: %s -> %s", r3.ID, rebasedRev.ID)
		}
		if len(rebasedSnap.Parents) != 1 || rebasedSnap.Parents[0] != s1.RevisionHash {
			t.Fatalf("rebased parent: %v", rebasedSnap.Parents)
		}
		// Use the rebased snapshot as the "current" tip going forward.
		s3 = rebasedSnap

		// ---- Phase 7: amend (content change, same revision id, new hash) ----
		buildFS(t, dir, "README.md", "hello world\n")
		// amend via CommitFromChanges on current revision.
		amendSnap, amendRev, err := ws.CommitFromChanges(
			s3.RevisionHash, []FileChangeSpec{{Path: "README.md", Content: []byte("hello world\n")}},
			"docs", store.Author{Name: "n"}, r3.ID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if amendRev.ID != r3.ID {
			t.Fatal("amend changed revision id")
		}
		if amendSnap.RevisionHash == s3.RevisionHash {
			t.Fatal("amend should change hash")
		}
		// Amend keeps r3's parents unchanged (still the rebased parent s1).
		if len(amendSnap.Parents) != 1 || amendSnap.Parents[0] != s1.RevisionHash {
			t.Fatalf("amend parents: %v", amendSnap.Parents)
		}

		// ---- Phase 8: conflict (merge two divergent) + resolve ----
		baseTree := object.NewTree()
		baseTree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("base"))}
		baseTreeID := ws.writeTree(baseTree)

		oursTree := object.NewTree()
		oursTree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("ours"))}
		oursID := ws.writeTree(oursTree)
		theirsTree := object.NewTree()
		theirsTree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("theirs"))}
		theirsID := ws.writeTree(theirsTree)

		mergedTree, atoms, err := ws.Merge(baseTreeID, oursID, theirsID)
		if err != nil {
			t.Fatal(err)
		}
		if len(atoms) != 1 || atoms[0].Path != "f.txt" {
			t.Fatalf("merge atoms: %d", len(atoms))
		}
		// Commit the merged (conflicted) tree directly as a new revision, then
		// resolve the conflict to "ours" and confirm the change id stays stable.
		confSnap := &store.Snapshot{
			RevisionID: mustRandomID(t), Parents: nil, TreeID: mergedTree,
			Description: "conflict", Author: store.Author{Name: "n"}, CommitTime: nowUtc(),
		}
		confSnap.RevisionHash = SnapshotID(confSnap)
		confRev := &store.Revision{ID: confSnap.RevisionID, Hash: confSnap.RevisionHash, Created: nowUtc()}
		if err := ws.storePut(confSnap, confRev); err != nil {
			t.Fatal(err)
		}

		// Resolve f.txt to side 0 (ours). Change id must be preserved.
		resSnap, resRev, err := ws.Resolve(confRev.ID, "f.txt", 0)
		if err != nil {
			t.Fatal(err)
		}
		if resRev.ID != confRev.ID {
			t.Fatalf("resolve changed revision id: %s -> %s", confRev.ID, resRev.ID)
		}
		rt := ws.mustTree(resSnap.TreeID)
		if rt.Entries["f.txt"].Kind == object.KindConflict {
			t.Fatal("conflict not resolved")
		}
		blob, _ := ws.ReadBlob(rt.Entries["f.txt"].ID)
		if string(blob) != "ours" {
			t.Fatalf("resolved content: %q", blob)
		}
		_ = mergedTree

		// ---- Phase 9: squash ----
		// Create a child then squash into parent.
		snapBase, revBase := commitTree(t, ws, dir, "base", nil)
		buildFS(t, dir, "extra.txt", "extra\n")
		snapChild, revChild := commitTree(t, ws, dir, "child", []object.ID{snapBase.RevisionHash})
		squashSnap, squashRev, err := ws.Squash(revChild.ID)
		if err != nil {
			t.Fatal(err)
		}
		if squashRev.ID != revBase.ID {
			t.Fatalf("squash should keep parent revision id: %s != %s", squashRev.ID, revBase.ID)
		}
		if squashSnap.RevisionHash == snapBase.RevisionHash {
			t.Fatal("squash should produce new hash")
		}
		// parent linkage preserved: squash result still points at base's parent (root -> none).
		if len(squashSnap.Parents) != 0 {
			t.Fatalf("squash of root child: parents %v", squashSnap.Parents)
		}
		_ = snapChild

		// ---- Phase 10: ignore-based tree build + ignore rewrite ----
		dir2 := t.TempDir()
		buildFS(t, dir2, "keep.txt", "k\n")
		buildFS(t, dir2, "secret.key", "s\n")
		buildFS(t, dir2, ".gitignore", "*.key\n")
		treeID, err := ws.BuildTreeFromFS(dir2)
		if err != nil {
			t.Fatal(err)
		}
		tree2 := ws.mustTree(treeID)
		if _, ok := tree2.Entries["secret.key"]; ok {
			t.Fatal("secret.key should be ignored by .gitignore")
		}
		if _, ok := tree2.Entries["keep.txt"]; !ok {
			t.Fatal("keep.txt should be present")
		}

		// Ignore rule change rewrites history to drop a now-ignored path.
		buildFS(t, dir, ".gitignore", "util.go\n*tmp\n")
		m, _ := ignore.New(dir)
		if m == nil {
			t.Fatal("matcher nil")
		}
		rewrote, err := ws.RewriteHistoryWithIgnores(dir, m, amendRev.ID)
		if err != nil {
			t.Fatal(err)
		}
		_ = rewrote
		curRev, _ := ws.GetRevision(amendRev.ID)
		curSnap, _ := ws.GetSnapshot(curRev.Hash)
		curTree := ws.mustTree(curSnap.TreeID)
		if srcEntry, ok := curTree.Entries["src"]; ok {
			srcTree := ws.mustTree(srcEntry.ID)
			if _, hasUtil := srcTree.Entries["util.go"]; hasUtil {
				t.Fatalf("util.go should be excluded after ignore rewrite")
			}
		}
	})
}
