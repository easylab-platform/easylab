package main

import (
	"os"
	"path/filepath"
	"testing"

	"easyvcs/internal/store"
)

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	return home
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustOpen(t *testing.T) *store.CentralStore {
	t.Helper()
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func firstSnapshotHash(t *testing.T, cs *store.CentralStore) string {
	t.Helper()
	repos, _ := cs.List()
	repo, _ := cs.OpenRepo(repos[0])
	revs, _ := repo.ListRevisions()
	snap, _ := repo.GetSnapshot(revs[0].Hash)
	return snap.RevisionHash.String()
}

func firstRevisionID(t *testing.T, cs *store.CentralStore) string {
	t.Helper()
	repos, _ := cs.List()
	repo, _ := cs.OpenRepo(repos[0])
	revs, _ := repo.ListRevisions()
	return revs[0].ID
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}
