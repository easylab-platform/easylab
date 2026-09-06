package oci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkr/pkrkit"
	"github.com/pkr/pkrkit/store"
)

func testRegistry(t *testing.T) *pkrkit.Registry {
	t.Helper()
	blobs, err := store.NewFileBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.OpenSQLite(t.TempDir() + "/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meta.Close() })
	return &pkrkit.Registry{
		Blobs: blobs, Meta: meta,
		Upstreams: &pkrkit.Upstreams{Defaults: map[string]string{"oci": "https://registry-1.docker.io"}, AirGap: true},
	}
}

func TestServeHTTPPingAndCatalog(t *testing.T) {
	reg := testRegistry(t)
	a := New(&OciState{Registry: reg})
	srv := httptest.NewServer(a)
	defer srv.Close()
	_ = context.Background()

	// ping
	resp, err := http.Get(srv.URL + "/v2")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Docker-Distribution-Api-Version") != "registry/2.0" {
		t.Fatalf("ping: %d %s", resp.StatusCode, resp.Header.Get("Docker-Distribution-Api-Version"))
	}
	resp.Body.Close()

	// catalog empty
	resp, _ = http.Get(srv.URL + "/v2/_catalog")
	body := readAll(t, resp)
	if !strings.Contains(body, `"repositories":[]`) {
		t.Fatalf("catalog: %s", body)
	}
	resp.Body.Close()

	// put blob single-shot
	data := []byte("hello oci")
	digest := pkrkit.DigestOf(data)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/myorg/myrepo/blobs/uploads/?digest="+digest, strings.NewReader(string(data)))
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if putResp.StatusCode != 201 {
		t.Fatalf("put blob: %d", putResp.StatusCode)
	}
	putResp.Body.Close()

	// head blob
	req, _ = http.NewRequest(http.MethodHead, srv.URL+"/v2/myorg/myrepo/blobs/"+digest, nil)
	headResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if headResp.StatusCode != 200 {
		t.Fatalf("head blob: %d", headResp.StatusCode)
	}
	headResp.Body.Close()

	// get blob
	getResp, err := http.Get(srv.URL + "/v2/myorg/myrepo/blobs/" + digest)
	if err != nil {
		t.Fatal(err)
	}
	got := readAll(t, getResp)
	if getResp.StatusCode != 200 || got != string(data) {
		t.Fatalf("get blob: %d %q", getResp.StatusCode, got)
	}
	getResp.Body.Close()
}

func TestSplitRegistry(t *testing.T) {
	cases := map[string][2]string{
		"ghcr.io/foo/bar": {"ghcr.io", "foo/bar"},
		"localhost:5000/x": {"localhost:5000", "x"},
		"library/alpine":   {"", "library/alpine"},
		"alpine":           {"", "alpine"},
	}
	for in, want := range cases {
		g, r := splitRegistry(in)
		if g != want[0] || r != want[1] {
			t.Fatalf("splitRegistry(%q) = (%q,%q), want %v", in, g, r, want)
		}
	}
}

func TestExtractBlobs(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:c1","size":1},"layers":[{"digest":"sha256:l1","size":2}]}`)
	blobs := extractBlobs(body)
	if len(blobs) != 2 || blobs[0].Digest != "sha256:c1" || blobs[1].Digest != "sha256:l1" {
		t.Fatalf("extractBlobs=%+v", blobs)
	}
	idx := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:m1","size":3}]}`)
	if b := extractBlobs(idx); len(b) != 1 || b[0].Digest != "sha256:m1" {
		t.Fatalf("index blobs=%+v", b)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
