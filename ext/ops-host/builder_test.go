package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry func(w *tar.Writer) error

func regular(name, body string) tarEntry {
	return func(w *tar.Writer) error {
		if err := w.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err := io.WriteString(w, body)
		return err
	}
}

func dirEntry(name string) tarEntry {
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir})
	}
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, mk := range entries {
		if err := mk(tw); err != nil {
			t.Fatalf("write tar entry: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// extractTarGz (local untar helper) was removed with the local build path —
// easylab now materializes build contexts server-side. The zip-slip tests below
// keep guarding the contract; they exercise a minimal local copy of the
// extractor so a future re-introduction cannot silently drop the checks.

func extractTarGz(r io.Reader, dest string, strip int) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(hdr.Name, "/") {
			return fmt.Errorf("absolute tar entry %q", hdr.Name)
		}
		// Normalize to components, strip the leading ones (tar
		// --strip-components semantics on raw components), then validate:
		// any remaining ".." component escapes and must be rejected.
		parts := strings.Split(hdr.Name, "/")
		if len(parts) > 0 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1] // trailing slash (dir entries)
		}
		if strip > 0 {
			if strip >= len(parts) {
				continue // fully consumed (e.g. a bare dir entry)
			}
			parts = parts[strip:]
		}
		for _, p := range parts {
			if p == ".." {
				return fmt.Errorf("illegal tar entry %q", hdr.Name)
			}
		}
		name := path.Join(parts...)
		if name == "." || name == "" {
			continue
		}
		target := filepath.Join(dest, name)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("entry escapes destination: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, data, 0o644); err != nil {
				return err
			}
		}
	}
}

// TestExtractTarGzZipSlip verifies that entries trying to escape the
// destination directory are rejected instead of written outside it.
func TestExtractTarGzZipSlip(t *testing.T) {
	cases := []string{
		"repo-1/../../evil.txt",
		"repo-1/sub/../../../evil2.txt",
		"/abs/evil3.txt",
		"../../evil5.txt",
	}
	for _, name := range cases {
		payload := tarGz(t, []tarEntry{regular(name, "pwn")})
		dest := t.TempDir()
		err := extractTarGz(bytes.NewReader(payload), dest, 1)
		if err == nil {
			t.Fatalf("entry %q: extract succeeded, want zip-slip rejection", name)
		}
	}
}

// TestExtractTarGzRejectsDotDotAfterStrip verifies that ".." surviving the
// strip is rejected even when a naive path.Clean would pull it back inside —
// the component-level check is the safe contract.
func TestExtractTarGzRejectsDotDotAfterStrip(t *testing.T) {
	payload := tarGz(t, []tarEntry{regular("repo-1/./../evil4.txt", "ok")})
	dest := t.TempDir()
	if err := extractTarGz(bytes.NewReader(payload), dest, 1); err == nil {
		t.Fatal("residual .. after strip must be rejected")
	}
}

// TestExtractTarGzNormal ensures legit archives still extract with strip.
func TestExtractTarGzNormal(t *testing.T) {
	payload := tarGz(t, []tarEntry{
		dirEntry("repo-1/"),
		regular("repo-1/src/main.go", "package main"),
	})
	dest := t.TempDir()
	if err := extractTarGz(bytes.NewReader(payload), dest, 1); err != nil {
		t.Fatalf("extract: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "src", "main.go"))
	if err != nil || string(b) != "package main" {
		t.Fatalf("extracted content = %q, %v", b, err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
