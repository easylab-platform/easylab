// Package store provides reference implementations of the pkrkit storage
// interfaces. It ships two blob stores — a filesystem CAS and a SQLite-BLOB
// CAS — plus a SQLite IndexStore, so a deployment can choose where each layer
// physically lives.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/pkr/pkrkit"
)

// FileBlobStore is a content-addressed store: one file per digest, addressed
// as `sha256/<first-two>/<rest>`. This is the default for large immutable
// layers/blobs because it keeps bytes out of the OS page cache worth of RAM
// and survives crashes with atomic rename.
type FileBlobStore struct {
	root string
	mu   sync.Mutex
}

// NewFileBlobStore opens a filesystem CAS rooted at dir.
func NewFileBlobStore(dir string) (*FileBlobStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileBlobStore{root: dir}, nil
}

func (s *FileBlobStore) path(digest string) (string, error) {
	hexpart, err := pkrkit.ParseDigest(digest)
	if err != nil {
		return "", err
	}
	if len(hexpart) < 3 {
		return "", errors.New("digest too short")
	}
	return filepath.Join(s.root, "sha256", hexpart[:2], hexpart[2:]), nil
}

// Stat implements BlobStore.
func (s *FileBlobStore) Stat(ctx context.Context, digest string) (*int64, error) {
	p, err := s.path(digest)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	n := fi.Size()
	return &n, nil
}

// Open implements BlobStore.
func (s *FileBlobStore) Open(ctx context.Context, digest string) (io.ReadSeekCloser, error) {
	p, err := s.path(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// PutIfAbsent implements BlobStore with atomic rename so a partially written
// file is never visible. Returns false when the digest already exists.
func (s *FileBlobStore) PutIfAbsent(ctx context.Context, digest string, r io.Reader) (bool, error) {
	hexpart, err := pkrkit.ParseDigest(digest)
	if err != nil {
		return false, err
	}
	dir := filepath.Join(s.root, "sha256", hexpart[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	final := filepath.Join(dir, hexpart[2:])

	s.mu.Lock()
	if _, err := os.Stat(final); err == nil {
		s.mu.Unlock()
		return false, nil // dedup
	}
	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	s.mu.Unlock()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		os.Remove(tmp.Name())
		return false, fmt.Errorf("digest mismatch: expected %s got %s", digest, got)
	}
	// Verify + publish under lock to avoid double-rename races.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(final); err == nil {
		os.Remove(tmp.Name())
		return false, nil
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	return true, nil
}

// HashesFor implements BlobStore: recompute (or read cached sidecar) from the
// stored bytes. We recompute every time — blobs are immutable and the cache is
// the file itself.
func (s *FileBlobStore) HashesFor(ctx context.Context, digest string) (pkrkit.Hashes, error) {
	f, err := s.Open(ctx, digest)
	if err != nil {
		return pkrkit.Hashes{}, err
	}
	if f == nil {
		return pkrkit.Hashes{}, pkrkit.ErrBlobUnknown
	}
	defer f.Close()
	h, err := pkrkit.ComputeHashes(f)
	if err != nil {
		return pkrkit.Hashes{}, err
	}
	return h, nil
}

// Delete implements BlobStore.
func (s *FileBlobStore) Delete(ctx context.Context, digest string) error {
	p, err := s.path(digest)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List implements BlobStore.
func (s *FileBlobStore) List(ctx context.Context) ([]string, error) {
	var out []string
	err := filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Name() == "sha256" {
			return nil
		}
		digest, ok := digestFromPath(s.root, path)
		if ok {
			out = append(out, digest)
		}
		return nil
	})
	return out, err
}

func digestFromPath(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	parts := splitPath(rel)
	if len(parts) != 2 {
		return "", false
	}
	if parts[0] != "sha256" || len(parts[1]) != 62 {
		return "", false
	}
	return "sha256:" + parts[1], true
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(p[i])
	}
	out = append(out, cur)
	return out
}
