package main

import (
	"fmt"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// mergeMergeRequest performs the single-parent (rebase) merge of a change
// request's source tree into its target, returning the new revision id,
// snapshot hash and conflict count. The source may live in a DIFFERENT
// repository (fork→upstream MR). Shared by the RPC merge path.
func (s *server) mergeMergeRequest(repo *store.Repo, mr *store.MergeRequest) (revID, snapshot string, conflicts int, err error) {
	ws := revision.NewWorkspace(repo)
	srcRepo := repo
	if mr.SourceRepoID != 0 && mr.SourceRepoID != repo.RepoID() {
		ref, rerr := s.cs.RepoRefByID(mr.SourceRepoID)
		if rerr != nil {
			return "", "", 0, fmt.Errorf("source repository missing: %w", rerr)
		}
		srcRepo, err = s.cs.OpenRepo(ref)
		if err != nil {
			return "", "", 0, fmt.Errorf("source repository missing: %w", err)
		}
	}
	srcTree, err := treeOf(ws, srcRepo, mr.Source)
	if err != nil {
		return "", "", 0, fmt.Errorf("source %s: %w", mr.Source, err)
	}
	tgtSnap, err := resolveSnapshotOf(ws, repo, mr.Target)
	if err != nil {
		return "", "", 0, fmt.Errorf("target %s: %w", mr.Target, err)
	}
	var base object.ID
	if len(tgtSnap.Parents) > 0 {
		base = tgtSnap.Parents[0]
	}
	mergedID, atoms, err := ws.Merge(base, srcTree, tgtSnap.TreeID)
	if err != nil {
		return "", "", 0, err
	}
	newSnap, _, err := ws.Commit(revision.CommitParams{
		Parents: []object.ID{tgtSnap.RevisionHash}, TreeID: mergedID,
		Description: "Merge " + mr.Source + " into " + mr.Target,
		Author:      store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
	})
	if err != nil {
		return "", "", 0, err
	}
	return newSnap.RevisionID, newSnap.RevisionHash.String(), len(atoms), nil
}
