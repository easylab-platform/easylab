// Package workspace materializes an easyvcs snapshot tree as a tar stream and
// unpacks such a stream on disk. It is the single place repo-tree export/
// import lives, shared by sandbox sync, CI build contexts and workspace
// seeding.
package workspace

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
)

// Tar packs the snapshot tree at root into an uncompressed tar, returning the
// archive and the file count. Paths are relative to the tree root.
func Tar(ws *revision.Workspace, root object.ID) ([]byte, int, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := 0
	var walk func(prefix string, id object.ID) error
	walk = func(prefix string, id object.ID) error {
		tree, err := ws.ReadTree(id)
		if err != nil {
			return err
		}
		for _, e := range tree.SortedEntries() {
			full := e.Name
			if prefix != "" {
				full = prefix + "/" + e.Name
			}
			switch e.Kind {
			case object.KindTree:
				if err := walk(full, e.ID); err != nil {
					return err
				}
			case object.KindBlob:
				data, err := ws.ReadBlob(e.ID)
				if err != nil {
					return err
				}
				hdr := &tar.Header{Name: full, Mode: 0o644, Size: int64(len(data))}
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				if _, err := tw.Write(data); err != nil {
					return err
				}
				files++
			}
		}
		return nil
	}
	if err := walk("", root); err != nil {
		return nil, 0, err
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}

// Extract unpacks a tarball into dir (regular files + directories). Paths are
// sanitized so the archive can never escape dir.
func Extract(dir string, tarball []byte) error {
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean("/" + hdr.Name)
		if clean == "/" || strings.Contains(clean, "..") {
			continue
		}
		target := filepath.Join(dir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777|0o400)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}
