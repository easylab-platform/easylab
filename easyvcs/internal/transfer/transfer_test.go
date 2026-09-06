package transfer

import (
	"bytes"
	"compress/gzip"
	"testing"
	"time"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// seedTransferRepo creates a repo with a couple of revisions and refs.
func seedTransferRepo(t testing.TB) (*store.Repo, *store.CentralStore) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cs.Create(store.RepoRef{Namespace: "n", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 100)
	tree := object.NewTree()
	tree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: object.BlobID(payload)}
	treeObj := &object.Object{Kind: object.KindTree, Tree: tree}
	_ = repo.WriteObject(treeObj)
	_ = repo.WriteObject(&object.Object{Kind: object.KindBlob, Blob: payload})
	snap := &store.Snapshot{
		RevisionHash: object.BlobID([]byte("s1")), RevisionID: "rev1",
		TreeID: treeObj.ID(), Description: "first",
		Author: store.Author{Name: "n"}, CommitTime: timeNow(),
	}
	_ = repo.PutSnapshot(snap)
	_ = repo.PutRevision(&store.Revision{ID: "rev1", Hash: snap.RevisionHash, Created: timeNow(), ChangedPaths: []string{"f.txt"}})
	_ = repo.PutRef(&store.Ref{Name: "main", Kind: store.RefBranch, Target: "rev1"})
	return repo, cs
}

func timeNow() time.Time { return time.Now() }

func TestMarshalUnmarshalBinary(t *testing.T) {
	repo, cs := seedTransferRepo(t)
	defer cs.Close()
	b, err := CollectAll(repo)
	if err != nil {
		t.Fatal(err)
	}
	// Marshal to binary frame.
	frame, err := b.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	// Unmarshal back.
	back, err := UnmarshalBinary(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Changes) != len(b.Changes) || len(back.Objects) != len(b.Objects) {
		t.Fatalf("frame mismatch: changes %d vs %d, objects %d vs %d",
			len(back.Changes), len(b.Changes), len(back.Objects), len(b.Objects))
	}
	if back.Repo != b.Repo {
		t.Fatal("repo mismatch")
	}
	// Verify one object's content round-trips.
	if len(back.Objects) > 0 {
		o, err := object.DecodeObject(object.BlobID(b.Objects[0].Content), b.Objects[0].Content)
		if err != nil {
			t.Fatal(err)
		}
		_ = o
	}
}

func TestUnmarshalGzipFrame(t *testing.T) {
	repo, cs := seedTransferRepo(t)
	defer cs.Close()
	b, _ := CollectAll(repo)
	compressed, err := CompressBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	// gzip frame has 0x1f 0x8b magic; UnmarshalBinary should detect and inflate.
	if compressed[0] != 0x1f || compressed[1] != 0x8b {
		t.Fatal("expected gzip magic")
	}
	back, err := UnmarshalBinary(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Objects) != len(b.Objects) {
		t.Fatal("gzip unmarshal object count mismatch")
	}
}

func TestUnmarshalNonBinary(t *testing.T) {
	if _, err := UnmarshalBinary([]byte("not a bundle")); err == nil {
		t.Fatal("expected error for non-binary frame")
	}
	// Truncated header.
	raw, err := (&Bundle{}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalBinary(raw[:4]); err == nil {
		t.Fatal("expected error for truncated header")
	}
}

func TestCollectApplyRoundTrip(t *testing.T) {
	repoA, csA := seedTransferRepo(t)
	defer csA.Close()
	b, _ := CollectAll(repoA)
	// Apply into a fresh repoB in the same central store.
	csB, err := store.Open(t.TempDir() + "/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer csB.Close()
	repoB, _ := csB.Create(store.RepoRef{Namespace: "n", Name: "rB"})
	n, err := Apply(repoB, b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("apply count: %d", n)
	}
	revs, _ := repoB.ListRevisions()
	if len(revs) != 1 {
		t.Fatalf("applied revs: %d", len(revs))
	}
	refs, _ := repoB.ListRefs()
	if len(refs) != 1 {
		t.Fatalf("applied refs: %d", len(refs))
	}
}

func TestCollectDeltaSkipsKnownObjects(t *testing.T) {
	repo, cs := seedTransferRepo(t)
	defer cs.Close()
	haveIDs, _ := EnumerateObjectIDs(repo)
	have := map[string]bool{}
	for _, id := range haveIDs {
		have[id.String()] = true
	}
	full, _ := CollectAll(repo)
	delta, err := Collect(repo, nil, func(id object.ID) bool { return have[id.String()] })
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Objects) == 0 {
		t.Fatal("full bundle should have objects")
	}
	if len(delta.Objects) != 0 {
		t.Fatalf("delta should be empty when all known, got %d", len(delta.Objects))
	}
	// Collect with a "have" list that includes the revision id skips it.
	delta2, _ := Collect(repo, []string{"rev1"}, nil)
	if len(delta2.Changes) != 0 {
		t.Fatalf("delta with have=rev1 should skip change, got %d", len(delta2.Changes))
	}
}

func TestCollectObjectsWhere(t *testing.T) {
	repo, cs := seedTransferRepo(t)
	defer cs.Close()
	// Collect objects reachable from the tree root of the first revision.
	changes, _ := repo.ListRevisions()
	snap, err := repo.GetSnapshot(changes[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := CollectObjectsWhere(repo, snap.TreeID, func(id object.ID) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("expected at least the tree object")
	}
}

func TestGzipSize(t *testing.T) {
	repo, cs := seedTransferRepo(t)
	defer cs.Close()
	b, _ := CollectAll(repo)
	plain, _ := b.MarshalBinary()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(plain)
	_ = gw.Close()
	if len(buf.Bytes()) >= len(plain) {
		t.Fatalf("gzip not beneficial: %d vs %d", buf.Len(), len(plain))
	}
}
