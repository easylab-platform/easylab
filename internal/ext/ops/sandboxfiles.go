package opsext

import (
	"context"
	"os"
	"time"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
)

// sandboxFileRead reads a sandbox file via the easylab gateway worker proxy
// (bytes, binary-safe).
func (s *server) sandboxFileRead(ctx context.Context, cid, path string) ([]byte, error) {
	res, err := s.sdk.Sandbox.FileRead(ctx, connect.NewRequest(&easylabv1.FileReadRequest{
		Sandbox: cid, Req: &workerv1.FileReadRequest{Path: path},
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetContent(), nil
}

// sandboxFileWrite writes a sandbox file (bytes; creates parent dirs).
func (s *server) sandboxFileWrite(ctx context.Context, cid, path string, data []byte) error {
	_, err := s.sdk.Sandbox.FileWrite(ctx, connect.NewRequest(&easylabv1.FileWriteRequest{
		Sandbox: cid, Req: &workerv1.FileWriteRequest{Path: path, Content: data},
	}))
	return err
}

// sandboxFileStat stats a sandbox path via the worker file_list RPC (`is_dir`
// drives the directory branch — for a single file is_dir is false).
func (s *server) sandboxFileStat(ctx context.Context, cid, path string) (os.FileInfo, error) {
	res, err := s.sdk.Sandbox.FileList(ctx, connect.NewRequest(&easylabv1.FileListRequest{
		Sandbox: cid, Req: &workerv1.FileListRequest{Path: path},
	}))
	if err != nil || res == nil || res.Msg == nil {
		return nil, err
	}
	return syntheticFileInfo{isDir: res.Msg.GetIsDir()}, nil
}

// sandboxFileList returns the files under a sandbox path (dir or single file)
// as {path, size, is_dir}.
func (s *server) sandboxFileList(ctx context.Context, cid, path string) ([]map[string]interface{}, error) {
	res, err := s.sdk.Sandbox.FileList(ctx, connect.NewRequest(&easylabv1.FileListRequest{
		Sandbox: cid, Req: &workerv1.FileListRequest{Path: path},
	}))
	if err != nil || res == nil || res.Msg == nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(res.Msg.GetFiles()))
	for _, e := range res.Msg.GetFiles() {
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
