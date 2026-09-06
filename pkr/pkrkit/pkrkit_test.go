package pkrkit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pkr/pkrkit"
	"github.com/pkr/pkrkit/store"
)

func TestFileBlobStoreDedupAndVerify(t *testing.T) {
	blobs, err := store.NewFileBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	data := []byte("hello pkr")
		d := pkrkit.DigestOf(data)
	ok, err := blobs.PutIfAbsent(ctx, d, strings.NewReader(string(data)))
	if err != nil || !ok {
		t.Fatalf("first put: %v ok=%v", err, ok)
	}
	// Dedup: second put is a no-op.
	ok, err = blobs.PutIfAbsent(ctx, d, strings.NewReader(string(data)))
	if err != nil || ok {
		t.Fatalf("second put should dedup, ok=%v err=%v", ok, err)
	}
	size, err := blobs.Stat(ctx, d)
	if err != nil || size == nil || *size != int64(len(data)) {
		t.Fatalf("stat: %v %v", size, err)
	}
	// Mismatched digest must be rejected.
	if _, err := blobs.PutIfAbsent(ctx, "sha256:"+strings.Repeat("0", 64), strings.NewReader(string(data))); err == nil {
		t.Fatal("expected digest mismatch")
	}
}

func TestSQLiteStoreRoundtrip(t *testing.T) {
	s, err := store.OpenSQLite(t.TempDir() + "/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	art := pkrkit.Artifact{
		Format: "oci", Repository: "library/alpine", Version: "3",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Proprietary: []byte(`{"x":1}`), Digest: "sha256:abc", Source: "push",
	}
	if err := s.Put(ctx, art); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "oci", "library/alpine", "3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != art.Digest || got.Version != "3" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	versions, _ := s.ListVersions(ctx, "oci", "library/alpine")
	if len(versions) != 1 || versions[0] != "3" {
		t.Fatalf("versions=%v", versions)
	}
	repos, _ := s.ListRepositoriesByFormat(ctx, "oci")
	if len(repos) != 1 || repos[0] != "library/alpine" {
		t.Fatalf("repos=%v", repos)
	}
	if err := s.Delete(ctx, "oci", "library/alpine", "3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "oci", "library/alpine", "3"); !errors.Is(err, pkrkit.ErrArtifactUnknown) {
		t.Fatalf("expected ErrArtifactUnknown, got %v", err)
	}
}

func TestTokenAuth(t *testing.T) {
	auth := pkrkit.NewTokenAuth("devtoken=write,readtoken=read")
	if _, ok := auth.CheckBearer(context.Background(), "devtoken", "repository:foo:pull"); !ok {
		t.Fatal("write token should permit pull")
	}
	if _, ok := auth.CheckBearer(context.Background(), "readtoken", "repository:foo:push"); ok {
		t.Fatal("read token should NOT permit push")
	}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer devtoken")
	if u := auth.Authenticate(context.Background(), req); u == "" {
		t.Fatal("bearer should authenticate")
	}
}

func TestUpstreamsProxyWalk(t *testing.T) {
	u := &pkrkit.Upstreams{Proxy: map[string]string{"nuget.search": "http://p:1", "nuget": "http://p2:2"}}
	if v, ok := u.ProxyURL("nuget.search"); !ok || v != "http://p:1" {
		t.Fatalf("exact: %v %v", v, ok)
	}
	if v, ok := u.ProxyURL("nuget.registration"); !ok || v != "http://p2:2" {
		t.Fatalf("parent walk: %v %v", v, ok)
	}
	if u.Get("npm") != "" || u.AirGap {
		t.Fatal("unconfigured format should be empty (no defaults here)")
	}
}

func TestMemCacheTTL(t *testing.T) {
	c := pkrkit.NewMemCache(20 * time.Millisecond)
	c.Set("k", "v")
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatal("cache miss on fresh key")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("cache should have expired")
	}
}
