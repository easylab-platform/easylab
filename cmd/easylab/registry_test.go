package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLabReleaseViaPkrkit verifies Lab releases are served by the artifactkit
// generic registry: creating a release writes a generic artifact keyed by
// repo, uploading an asset stores a content-addressed blob, and the download
// returns the original bytes.
func TestLabReleaseViaPkrkit(t *testing.T) {
	s := newLabServer(t)
	seedTestToken(t, s, "t")
	c := &labClient{t: t, s: s, token: "t"}
	c.ok("POST", "/api/v1/repo", map[string]any{"namespace": "team", "name": "app"})

	// Create a release at tag v1.0.0.
	rel := c.ok("POST", "/api/v1/repo/team/app/releases", map[string]any{
		"tag": "v1.0.0", "name": "v1", "description": "first release",
	})
	if rel["tag"] != "v1.0.0" {
		t.Fatalf("create release: %v", rel)
	}

	// Upload an asset via multipart.
	content := []byte("artifact-bytes-123")
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", "app.tar.gz")
	fw.Write(content)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/repo/team/app/releases/v1.0.0/assets", &b)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload asset: %d %s", rec.Code, rec.Body.String())
	}
	var asset map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &asset)
	if asset["name"] != "app.tar.gz" || asset["size"] != float64(len(content)) {
		t.Fatalf("asset: %v", asset)
	}

	// List assets.
	al := c.do("GET", "/api/v1/repo/team/app/releases/v1.0.0/assets", nil)
	if al.Code != http.StatusOK {
		t.Fatalf("list assets: %d", al.Code)
	}
	var assets []map[string]any
	_ = json.Unmarshal(al.Body.Bytes(), &assets)
	if len(assets) != 1 || assets[0]["name"] != "app.tar.gz" {
		t.Fatalf("assets: %v", assets)
	}

	// Download the asset by name -> original bytes.
	dl := c.do("GET", "/api/v1/repo/team/app/releases/v1.0.0/assets/app.tar.gz", nil)
	if dl.Code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), content) {
		t.Fatalf("download: %d %q", dl.Code, dl.Body.String())
	}

	// List releases.
	rl := c.do("GET", "/api/v1/repo/team/app/releases", nil)
	if rl.Code != http.StatusOK {
		t.Fatalf("list releases: %d", rl.Code)
	}
	var rels []map[string]any
	_ = json.Unmarshal(rl.Body.Bytes(), &rels)
	if len(rels) != 1 || rels[0]["tag"] != "v1.0.0" {
		t.Fatalf("releases: %v", rels)
	}
}

// TestLabGenericPackageProxy verifies the artifactkit generic protocol is mounted at
// /pkgs/generic and round-trips a raw artifact: PUT then GET.
func TestLabGenericPackageProxy(t *testing.T) {
	s := newLabServer(t)
	seedTestToken(t, s, "t")

	// PUT a raw artifact at /pkgs/generic/team:repo/v1.0.0/file.txt.
	body := []byte("hello generic")
	req := httptest.NewRequest("PUT", "/pkgs/generic/team:repo/v1.0.0/file.txt", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("generic put: %d %s", rec.Code, rec.Body.String())
	}

	// GET it back.
	req2 := httptest.NewRequest("GET", "/pkgs/generic/team:repo/v1.0.0/file.txt", nil)
	rec2 := httptest.NewRecorder()
	s.router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || !bytes.Equal(rec2.Body.Bytes(), body) {
		t.Fatalf("generic get: %d %q", rec2.Code, rec2.Body.String())
	}
}
