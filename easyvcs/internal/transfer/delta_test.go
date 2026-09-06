package transfer

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/revision"
	"easyvcs/internal/store"
)

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(data)
	_ = gw.Close()
	return buf.Bytes()
}

// TestFetchDeltaSkipsKnownObjects verifies the object-level want/have: when the
// destination already owns an object id, Collect omits it, so the wire payload
// shrinks to only the objects that are genuinely new.
func TestFetchDeltaSkipsKnownObjects(t *testing.T) {
	repo, cs, ws := newTestRepo3(t)
	defer cs.Close()
	seedThreeChanges(t, repo, ws)

	// Enumerate the object ids we "already have".
	haveIDs, err := EnumerateObjectIDs(repo)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, id := range haveIDs {
		have[id.String()] = true
	}

	// Full bundle contains N objects.
	full, err := CollectAll(repo)
	if err != nil {
		t.Fatal(err)
	}
	fullCount := countObjects(full)

	// Delta with all objects known -> zero objects.
	delta, err := Collect(repo, nil, func(id object.ID) bool { return have[id.String()] })
	if err != nil {
		t.Fatal(err)
	}
	deltaCount := countObjects(delta)
	if deltaCount != 0 {
		t.Fatalf("expected 0 objects when all known, got %d", deltaCount)
	}
	if fullCount == 0 {
		t.Fatalf("full bundle unexpectedly empty")
	}
	t.Logf("want/have works: full=%d objects, delta(known)=%d objects", fullCount, deltaCount)
}

func countObjects(b *Bundle) int { return len(b.Objects) }

// newTestRepo3 is a helper that seeds a repo and returns a workspace.
func newTestRepo3(t *testing.T) (*store.Repo, *store.CentralStore, *revision.Workspace) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cs.Create(store.RepoRef{Namespace: "test", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	return repo, cs, revision.NewWorkspace(repo)
}

func seedThreeChanges(t *testing.T, repo *store.Repo, ws *revision.Workspace) {
	t.Helper()
	for i := 0; i < 3; i++ {
		payload := []byte{byte('A' + i)}
		tree := object.NewTree()
		tree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: object.BlobID(payload)}
		treeObj := &object.Object{Kind: object.KindTree, Tree: tree}
		if err := repo.WriteObject(treeObj); err != nil {
			t.Fatal(err)
		}
		if err := repo.WriteObject(&object.Object{Kind: object.KindBlob, Blob: payload}); err != nil {
			t.Fatal(err)
		}
		snap, ch, err := ws.Commit(revision.CommitParams{
			TreeID:      treeObj.ID(),
			Description: "c",
			Author:      store.Author{Name: "t"},
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = snap
		_ = ch
	}
}

// TestGzipBundleSize verifies gzip shrinks the JSON bundle dramatically.
func TestGzipBundleSize(t *testing.T) {
	repo, cs, ws := newTestRepo3(t)
	defer cs.Close()
	seedThreeChanges(t, repo, ws)
	bundle, err := CollectAll(repo)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(bundle)
	plainSize := len(plain)
	gz := gzipBytes(plain)
	if len(plain) == 0 {
		t.Fatal("empty bundle")
	}
	// gzip should always be smaller here (large base64 payloads compress well).
	if len(gz) >= plainSize {
		t.Fatalf("gzip not beneficial: plain=%d gzip=%d", plainSize, len(gz))
	}
	t.Logf("plain=%d bytes, gzip=%d bytes, ratio=%.2f", plainSize, len(gz), float64(len(gz))/float64(plainSize))
}
