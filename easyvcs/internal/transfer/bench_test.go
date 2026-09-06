package transfer

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"testing"
	"time"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// buildRepo seeds a repo with many DISTINCT blobs committed as changes, so the
// bundle actually collects objects (a change's snapshot references its tree).
func buildRepo(tb testing.TB, n int) (*store.Repo, *store.CentralStore) {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		tb.Fatal(err)
	}
	repo, err := cs.Create(store.RepoRef{Namespace: "bench", Name: "repo"})
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		payload := bytes.Repeat([]byte{byte('A' + i%26), byte('a' + i%17)}, 3000) // ~6KB distinct blob
		tree := object.NewTree()
		tree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: object.BlobID(payload)}
		treeObj := &object.Object{Kind: object.KindTree, Tree: tree}
		_ = repo.WriteObject(treeObj)
		_ = repo.WriteObject(&object.Object{Kind: object.KindBlob, Blob: payload})
		snap := &store.Snapshot{
			RevisionHash: object.BlobID([]byte(payload)),
			RevisionID:   string(rune('a' + i)),
			TreeID:       treeObj.ID(),
			Description:  "c",
			CommitTime:   time.Unix(0, int64(i)),
		}
		_ = repo.PutSnapshot(snap)
		_ = repo.PutRevision(&store.Revision{ID: snap.RevisionID, Hash: snap.RevisionHash})
	}
	return repo, cs
}

// BenchmarkCollectFull measures the cost of gathering a full bundle (the
// push path when the peer has nothing).
func BenchmarkCollectFull(b *testing.B) {
	repo, cs := buildRepo(b, 20)
	defer cs.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := CollectAll(repo)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCollectDeltaSkip measures the cost of collecting a bundle when the
// peer already owns most objects (so they are skipped).
func BenchmarkCollectDeltaSkip(b *testing.B) {
	repo, cs := buildRepo(b, 20)
	defer cs.Close()
	// Pretend the peer has all ids: hasObject always true -> skip everything.
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Collect(repo, nil, func(id object.ID) bool { return true })
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONSize reports the serialized size of a full bundle as plain JSON.
func BenchmarkJSONSize(b *testing.B) {
	repo, cs := buildRepo(b, 20)
	defer cs.Close()
	bundle, err := CollectAll(repo)
	if err != nil {
		b.Fatal(err)
	}
	plain, _ := json.Marshal(bundle)
	plainSize := len(plain)

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(plain)
	_ = gw.Close()
	gzSize := gz.Len()

	b.ReportMetric(float64(plainSize)/1024, "plain-kb")
	b.ReportMetric(float64(gzSize)/1024, "gzip-kb")
	b.Logf("size: plain=%d bytes gzip=%d bytes ratio=%.2f", plainSize, gzSize, float64(gzSize)/float64(plainSize))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(bundle)
	}
}
