package main

import (
	"fmt"

	"github.com/easylab-platform/easylab/internal/workspace"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// exportWorkspace resolves org/repo@branch and returns the branch tree as a
// tar stream (plus the resolved tree id). It is the single repo-tree export
// used by sandbox sync, CI build contexts and workspace seeding.
func (s *server) exportWorkspace(org, repoName, branch string) (string, []byte, error) {
	r, err := s.cs.OpenRepo(store.RepoRef{Namespace: org, Name: repoName})
	if err != nil {
		return "", nil, fmt.Errorf("open %s/%s: %w", org, repoName, err)
	}
	ws := revision.NewWorkspace(r)
	treeID, err := treeOfRef(ws, r, branch)
	if err != nil {
		return "", nil, fmt.Errorf("ref %s: %w", branch, err)
	}
	tar, _, err := workspace.Tar(ws, treeID)
	if err != nil {
		return "", nil, err
	}
	return treeID.String(), tar, nil
}
