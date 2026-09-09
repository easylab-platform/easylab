package opsext

import (
	"context"
	"os"
	"time"

	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
)

// sandboxFileRead reads a sandbox file via the easylab gateway worker proxy
// (bytes, binary-safe).
func (s *server) sandboxFileRead(ctx context.Context, cid, path string) ([]byte, error) {
	res, err := s.sdk.Sandbox().FileRead(ctx, cid, &workerv1.FileReadRequest{Path: path})
	if err != nil {
		return nil, err
	}
	return res.Msg.Content, nil
}

// sandboxFileWrite writes a sandbox file (bytes; creates parent dirs).
func (s *server) sandboxFileWrite(ctx context.Context, cid, path string, data []byte) error {
	_, err := s.sdk.Sandbox().FileWrite(ctx, cid, &workerv1.FileWriteRequest{Path: path, Content: data})
	return err
}

// sandboxFileStat stats a sandbox path via the worker file_list RPC (`is_dir`
// drives the directory branch — for a single file is_dir is false).
func (s *server) sandboxFileStat(ctx context.Context, cid, path string) (os.FileInfo, error) {
	res, err := s.sdk.Sandbox().FileList(ctx, cid, &workerv1.FileListRequest{Path: path})
	if err != nil || res == nil || res.Msg == nil {
		return nil, err
	}
	return syntheticFileInfo{isDir: res.Msg.IsDir}, nil
}

// sandboxFileList returns the files under a sandbox path (dir or single file)
// as {path, size, is_dir}.
func (s *server) sandboxFileList(ctx context.Context, cid, path string) ([]map[string]interface{}, error) {
	res, err := s.sdk.Sandbox().FileList(ctx, cid, &workerv1.FileListRequest{Path: path})
	if err != nil || res == nil || res.Msg == nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(res.Msg.Files))
	for _, e := range res.Msg.Files {
		out = append(out, map[string]interface{}{
			"path": e.Path, "size": e.Size, "is_dir": e.IsDir,
		})
	}
	return out, nil
}

// syntheticFileInfo is a minimal os.FileInfo for sandboxFileStat.
type syntheticFileInfo struct{ isDir bool }

func (s syntheticFileInfo) Name() string       { return "" }
func (s syntheticFileInfo) Size() int64        { return 0 }
func (s syntheticFileInfo) Mode() os.FileMode  { return 0 }
func (s syntheticFileInfo) ModTime() time.Time { return time.Time{} }
func (s syntheticFileInfo) IsDir() bool        { return s.isDir }
func (s syntheticFileInfo) Sys() interface{}   { return nil }
