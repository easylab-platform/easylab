package oci

import (
	"fmt"
	"io"
	"os"
)

// tmpFile is a temp file that is always removed when dropped.
type tmpFile struct {
	path string
	f    *os.File
}

func newTempFile() (*tmpFile, error) {
	f, err := os.CreateTemp("", "pkr-oci-cache-*")
	if err != nil {
		return nil, err
	}
	return &tmpFile{path: f.Name(), f: f}, nil
}

func (t *tmpFile) Close() error {
	if t.f != nil {
		t.f.Close()
	}
	if t.path != "" {
		os.Remove(t.path)
	}
	return nil
}

func (t *tmpFile) pathStr() string { return t.path }

func newTempWriter(path string) (io.WriteCloser, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func fileRead(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read temp blob: %w", err)
	}
	return b, nil
}
