// Command easyvcs is the EasyVCS command-line interface.
//
// EasyVCS uses a central SQLite database (default ~/.easyvcs/easyvcs.db,
// overridable via EASYVCS_HOME) holding all repositories. A working directory
// is bound to a repository via a .easyvcs-workspace pointer file. Commands are
// minimal and mirror the change-native model: committing creates a snapshot
// under a stable change id; rebase repoints a change's parents changing only
// the snapshot's sha.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"easyvcs/internal/ignore"
	"easyvcs/internal/object"
	"easyvcs/internal/revision"
	"easyvcs/internal/store"
)

// HasIgnores reports whether the current directory has any ignore files.
func HasIgnores() bool {
	_, err := os.Stat(".gitignore")
	if err == nil {
		return true
	}
	_, err = os.Stat(".vcsignore")
	return err == nil
}

const usage = `easyvcs - a revision-native version control system (central-store)

Repository / workspace:
  init <ns/name> [DIR]      create a repository and a workspace pointing at it
  workspace attach [DIR]    bind current dir to a repo (auto-forks on mismatch)
  workspace list            list registered workspaces
  repositories              list all repositories in the central store

Revisions:
  commit [--new] [DIR]      create a snapshot (default: amend current revision)
  amend [DIR]               update the workspace's current revision snapshot
  log [DIR]                 list revisions and their current snapshot
  show <sha> [DIR]          show a snapshot's metadata
  checkout <sha> [DIR]      set current_revision + materialize tree into dir
  rebase <revision> [--onto <parent>]...  repoint a change onto one or more parents
  squash <revision> [DIR]     absorb a change into its parent (parent id stable)
  resolve <revision> <path> [--side N]  resolve a conflict to a chosen side

References:
  branch <name> <revision> [DIR]  set a mutable branch -> change
  tag <name> <revision> [DIR]       set an immutable tag -> change
  refs [DIR]                list all branchs and tags

  log [--count] <path>       list revisions that modified a file (or count)

Content:
  diff <shaA> <shaB> [DIR]  list file differences between two snapshot trees

Portable:
  export <ns/name> -o FILE  export a repository to a single .evcs file
  import FILE [-n ns]       import a repository from a .evcs file

Network (own smart protocol):
  remote add <name> <url>   register a remote server URL
  remote                    list remotes
  fetch <remote> [ref...]   fetch all (or given) remote branchs into origin/*
  pull <remote> [branch]  fetch + merge a remote branch into local (default branch)
  push <remote>             push changes to an EasyVCS server

Git interop (no smart protocol, full + minimal-diff commit):
  git-pull <git-url>        fetch a git repo as one local change
  git-push <git-url>        push current revision as one git commit

Flags:
  -help, -h                 show this help
`

type ctx struct {
	cs *store.CentralStore
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(0)
	}
	cmd := os.Args[1]
	switch cmd {
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
		os.Exit(0)
	}

	cs, err := store.OpenDefault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	_ = cs.SetWAL()
	c := &ctx{cs: cs}

	switch cmd {
	case "init":
		cmdInit(c)
	case "workspace":
		cmdWorkspace(c)
	case "repositories":
		cmdRepositories(c)
	case "commit":
		cmdCommit(c)
	case "amend":
		cmdAmend(c)
	case "log":
		// log [--count] <path>: report revisions that modified a file.
		if len(os.Args) >= 3 {
			count := false
			pathArg := ""
			for _, a := range os.Args[2:] {
				if a == "--count" || a == "-count" || a == "-n" {
					count = true
				} else if pathArg == "" {
					pathArg = a
				}
			}
			if pathArg != "" {
				cmdLogPath(c, pathArg, count)
				return
			}
		}
		cmdLog(c)
	case "show":
		cmdShow(c)
	case "checkout":
		cmdCheckout(c)
	case "rebase":
		cmdRebase(c)
	case "squash":
		cmdSquash(c)
	case "resolve":
		cmdResolve(c)
	case "branch":
		cmdBranch(c)
	case "tag":
		cmdTag(c)
	case "refs":
		cmdRefs(c)
	case "diff":
		cmdDiff(c)
	case "export":
		cmdExport(c)
	case "import":
		cmdImport(c)
	case "remote":
		cmdRemote(c)
	case "fetch":
		cmdFetch(c)
	case "pull":
		cmdPull(c)
	case "push":
		cmdPush(c)
	case "git-pull":
		cmdGitPull(c)
	case "git-push":
		cmdGitPush(c)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(1)
	}
}

// resolveWorkingDir returns the directory a command should operate on.
func resolveWorkingDir() string {
	return "."
}

// loadRepo resolves the workspace for the current directory and returns the
// repo-scoped handle plus the workspace marker.
func (c *ctx) loadRepo() (*store.Repo, *store.WorkspaceMarker, error) {
	marker, err := store.LookupMarker(".")
	if err != nil {
		return nil, nil, err
	}
	repo, err := c.cs.OpenRepo(marker.Repo)
	if err != nil {
		return nil, nil, err
	}
	return repo, marker, nil
}

func cmdInit(c *ctx) {
	args := os.Args[2:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: init <ns/name> [DIR]")
		os.Exit(1)
	}
	ns, name, err := splitRepo(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	dir := "."
	if len(args) > 1 {
		dir = args[1]
	}
	repo, err := c.cs.Create(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	if err := store.WriteMarker(dir, &store.WorkspaceMarker{Repo: repo.RepoRef()}); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	fmt.Printf("initialized repository %s/%s, workspace bound at %s\n", ns, name, dir)
}

func cmdWorkspace(c *ctx) {
	args := os.Args[2:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: workspace attach|open|list")
		os.Exit(1)
	}
	switch args[0] {
	case "attach", "open":
		cmdWorkspaceAttach(c)
	case "list":
		cmdWorkspaceList(c)
	default:
		fmt.Fprintf(os.Stderr, "unknown workspace subcommand %q\n", args[0])
		os.Exit(1)
	}
}

// cmdWorkspaceAttach binds the current directory to a repository. It reads (or
// creates) a workspace marker, resolves the repo, and if the recorded current
// change no longer exists it auto-forks a new tip change (plan A). It only
// runs on explicit attach, never implicitly during other commands.
func cmdWorkspaceAttach(c *ctx) {
	marker, err := store.LookupMarker(".")
	if err != nil {
		// No marker yet: create from a repo arg if provided.
		args := os.Args[2:]
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: workspace attach <ns/name> [DIR]")
			os.Exit(1)
		}
		ns, name, err := splitRepo(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "workspace attach:", err)
			os.Exit(1)
		}
		repo, err := c.cs.OpenRepo(store.RepoRef{Namespace: ns, Name: name})
		if err != nil {
			fmt.Fprintln(os.Stderr, "workspace attach:", err)
			os.Exit(1)
		}
		marker = &store.WorkspaceMarker{Repo: repo.RepoRef()}
		if err := store.WriteMarker(".", marker); err != nil {
			fmt.Fprintln(os.Stderr, "workspace attach:", err)
			os.Exit(1)
		}
		fmt.Printf("workspace attached to %s\n", repo)
		return
	}
	repo, err := c.cs.OpenRepo(marker.Repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "workspace attach: repository not found:", marker.Repo, "(import it first)")
		os.Exit(1)
	}
	// If current_revision is set but missing, auto-fork a new tip.
	if marker.CurrentRevision != "" {
		_, err := repo.GetRevision(marker.CurrentRevision)
		if err != nil {
			fmt.Printf("detected current revision %s missing; forking new tip from workspace\n", short(marker.CurrentRevision))
			ws := revision.NewWorkspace(repo)
			treeID, err := ws.BuildTreeFromFS(".")
			if err != nil {
				fmt.Fprintln(os.Stderr, "workspace attach:", err)
				os.Exit(1)
			}
			// Filter the new tree with the ignore matcher before committing.
			m, _ := ignore.New(".")
			if HasIgnores() {
				treeID, err = ws.RebuildTreeFiltered(".", m, treeID)
				if err != nil {
					fmt.Fprintln(os.Stderr, "workspace attach:", err)
					os.Exit(1)
				}
			}
			// New change on top of the repo tip (first branch or root).
			parents := repoTip(repo)
			snap, ch, err := ws.Commit(revision.CommitParams{
				Parents:     parents,
				TreeID:      treeID,
				Description: "fork",
				Author:      store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "workspace attach:", err)
				os.Exit(1)
			}
			marker.CurrentRevision = ch.ID
			_ = snap
			if err := store.WriteMarker(".", marker); err != nil {
				fmt.Fprintln(os.Stderr, "workspace attach:", err)
				os.Exit(1)
			}
			fmt.Printf("forked new revision %s -> snapshot %s\n", short(ch.ID), short(snap.RevisionHash.String()))
		}
	}
	if err := c.cs.PutWorkspace(&store.WorkspaceRow{
		Path: ".", Repo: marker.Repo, CurrentRevision: marker.CurrentRevision, Branch: marker.Branch,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "workspace attach:", err)
		os.Exit(1)
	}
	fmt.Printf("workspace bound to %s (current revision %s)\n", repo, short(marker.CurrentRevision))
}

func cmdWorkspaceList(c *ctx) {
	ws, err := c.cs.ListWorkspaces()
	if err != nil {
		fmt.Fprintln(os.Stderr, "workspace list:", err)
		os.Exit(1)
	}
	if len(ws) == 0 {
		fmt.Println("no workspaces")
		return
	}
	for _, w := range ws {
		fmt.Printf("%s  ->  %s  (%s)\n", w.Path, w.Repo, short(w.CurrentRevision))
	}
}

func cmdRepositories(c *ctx) {
	repos, err := c.cs.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "repositories:", err)
		os.Exit(1)
	}
	if len(repos) == 0 {
		fmt.Println("no repositories")
		return
	}
	for _, r := range repos {
		fmt.Println(r)
	}
}

func cmdCommit(c *ctx) {
	repo, marker, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "commit:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)
	dir := resolveWorkingDir()

	// --new creates a fresh revision; otherwise commit AMENDS onto the current
	// revision (the workspace's existing change), keeping revision_id stable.
	// This matches the jj change model: edit repeatedly, "new" to split.
	isNew := false
	for _, a := range os.Args[2:] {
		if a == "--new" || a == "-new" {
			isNew = true
		}
	}

	// Resolve the parent snapshot hash (current revision). Empty for a root
	// commit.
	var parentHash object.ID
	if marker.CurrentRevision != "" {
		cur, err := ws.GetRevision(marker.CurrentRevision)
		if err == nil {
			parentHash = cur.Hash
		}
	}

	// Read-only scan of the working directory: compute the diff against the
	// parent tree WITHOUT persisting the whole tree. Only the changed paths are
	// produced as file changes.
	changes, err := ws.ComputeChangesFromDir(parentHash, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "commit:", err)
		os.Exit(1)
	}

	// Atomic commit from the computed changes (same code path as the server
	// /files API). Default amends onto the current revision (revision_id stable);
	// --new creates a fresh revision chain from the current tip.
	revisionID := ""
	if !isNew && marker.CurrentRevision != "" {
		revisionID = marker.CurrentRevision
	}
	snap, ch, err := ws.CommitFromChanges(
		parentHash, changes, "commit",
		store.Author{Name: "easyvcs", Email: "easyvcs@example.com"}, revisionID,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "commit:", err)
		os.Exit(1)
	}
	if !isNew && marker.CurrentRevision != "" {
		fmt.Printf("amended revision %s -> snapshot %s\n", short(ch.ID), short(snap.RevisionHash.String()))
	} else {
		fmt.Printf("revision %s -> snapshot %s\n", short(ch.ID), short(snap.RevisionHash.String()))
	}
	// Detect a change in ignore files vs the previous snapshot. If the matcher
	// now ignores paths that were previously tracked in any ancestor, rewrite the
	// history to exclude them and auto-rebase descendants.
	if HasIgnores() {
		if err := applyIgnoreRewrite(c, repo, ws, ch.ID, marker); err != nil {
			fmt.Fprintln(os.Stderr, "commit (ignore rewrite):", err)
			os.Exit(1)
		}
	}
	// Update the workspace pointer to the current revision.
	marker.CurrentRevision = ch.ID
	if err := store.WriteMarker(".", marker); err != nil {
		fmt.Fprintln(os.Stderr, "commit:", err)
		os.Exit(1)
	}
	_ = c.cs.PutWorkspace(&store.WorkspaceRow{Path: ".", Repo: marker.Repo, CurrentRevision: ch.ID, Branch: marker.Branch})
}

// applyIgnoreRewrite checks whether ignore rules changed; if so it removes
// now-ignored paths from all ancestor snapshots and rebases descendants to the
// rewritten parents. It uses the workspace's current snapshot (just committed)
// as the tip whose ancestry is rewritten.
func applyIgnoreRewrite(c *ctx, repo *store.Repo, ws *revision.Workspace, changeID string, marker *store.WorkspaceMarker) error {
	// Compute the matcher and a hash of ignore files at the working dir.
	m, err := ignore.New(".")
	if err != nil {
		return err
	}
	curHash, err := revision.IgnoreHash(".")
	if err != nil {
		return err
	}
	// Find the previously recorded ignore hash, if any (stored in the workspaces
	// row / marker). If none or changed, run the rewrite.
	prevHash := marker.IgnoreHash
	if prevHash == curHash {
		return nil // no ignore rules changed
	}
	if prevHash == "" {
		// No baseline: only rewrite if this is not the very first commit (i.e.
		// there is a parent whose tree still contains ignored paths). We still do
		// a targeted filter on the current commit's ancestors to be safe.
	}

	rewrote, err := ws.RewriteHistoryWithIgnores(".", m, changeID)
	if err != nil {
		return err
	}
	if rewrote > 0 {
		fmt.Printf("ignore rules changed; excluded now-ignored paths from %d snapshot(s) and rebased descendants\n", rewrote)
	}
	// Persist the new ignore hash in the workspace marker.
	marker.IgnoreHash = curHash
	return nil
}

func cmdAmend(c *ctx) {
	repo, marker, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amend:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)
	dir := resolveWorkingDir()
	revisionID := marker.CurrentRevision
	cur, err := repo.GetRevision(revisionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amend: no current revision:", err)
		os.Exit(1)
	}
	curSnap, err := repo.GetSnapshot(cur.Hash)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amend:", err)
		os.Exit(1)
	}
	// Compute the diff against the current snapshot's tree using a read-only
	// scan (no full-tree persistence). The amend rewrites the current revision.
	changes, err := ws.ComputeChangesFromDir(cur.Hash, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amend:", err)
		os.Exit(1)
	}
	ns, ch, err := ws.CommitFromChanges(
		cur.Hash, changes,
		curSnap.Description, curSnap.Author, revisionID,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amend:", err)
		os.Exit(1)
	}
	// Amend keeps the same revision_id; repoint it to the new hash.
	if err := repo.UpdateRevisionHash(ch.ID, ns.RevisionHash); err != nil {
		fmt.Fprintln(os.Stderr, "amend:", err)
		os.Exit(1)
	}
	_ = ns
	fmt.Printf("amended revision %s -> snapshot %s\n", short(ch.ID), short(ns.RevisionHash.String()))
}

func cmdLog(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "log:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)
	revs, err := ws.Log()
	if err != nil {
		fmt.Fprintln(os.Stderr, "log:", err)
		os.Exit(1)
	}
	for _, ch := range revs {
		snap, err := ws.GetSnapshot(ch.Hash)
		if err != nil {
			fmt.Fprintf(os.Stderr, "log: %v\n", err)
			continue
		}
		fmt.Printf("%s  %s\n", short(ch.ID), short(snap.RevisionHash.String()))
		fmt.Printf("    %s\n", snap.Description)
	}
}

// cmdLogPath reports the revisions that modified a given path, along with the
// count. It walks the DAG from the current revision upward and filters using
// each revision's ChangedPaths (cheap), then compares blob hashes to confirm
// an actual content change.
func cmdLogPath(c *ctx, path string, countOnly bool) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "log:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)
	edits, err := ws.FileHistory("", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "log:", err)
		os.Exit(1)
	}
	if countOnly {
		fmt.Println(len(edits))
		return
	}
	for _, e := range edits {
		fmt.Printf("%s  %s\n", short(e.RevisionID), short(e.RevisionHash.String()))
		fmt.Printf("    %s\n", e.Timestamp.Format("2006-01-02 15:04:05"))
	}
}

func cmdShow(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "show:", err)
		os.Exit(1)
	}
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: show <sha>")
		os.Exit(1)
	}
	id, err := object.HexToID(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "show:", err)
		os.Exit(1)
	}
	snap, err := repo.GetSnapshot(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "show:", err)
		os.Exit(1)
	}
	fmt.Printf("snapshot  %s\n", snap.RevisionHash)
	fmt.Printf("revision  %s\n", snap.RevisionID)
	fmt.Printf("tree      %s\n", snap.TreeID)
	for _, p := range snap.Parents {
		fmt.Printf("parent    %s\n", p)
	}
	fmt.Printf("author    %s\n", snap.Author)
	fmt.Printf("description %s\n", snap.Description)
}

func cmdCheckout(c *ctx) {
	repo, marker, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkout:", err)
		os.Exit(1)
	}
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: checkout <sha> [DEST]")
		os.Exit(1)
	}
	id, err := object.HexToID(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkout:", err)
		os.Exit(1)
	}
	snap, err := repo.GetSnapshot(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkout: unknown snapshot:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)
	dest := ""
	if len(os.Args) > 3 {
		dest = os.Args[3]
		if !filepath.IsAbs(dest) {
			wd, _ := os.Getwd()
			dest = filepath.Join(wd, dest)
		}
	} else {
		dest = "."
	}
	if err := ws.Materialize(snap.TreeID, dest); err != nil {
		fmt.Fprintln(os.Stderr, "checkout:", err)
		os.Exit(1)
	}
	// Checkout repoints the workspace's current_change only (does not touch db).
	marker.CurrentRevision = snap.RevisionID
	_ = store.WriteMarker(".", marker)
	_ = c.cs.PutWorkspace(&store.WorkspaceRow{Path: ".", Repo: marker.Repo, CurrentRevision: snap.RevisionID, Branch: marker.Branch})
	fmt.Printf("checked out snapshot %s (revision %s)\n", short(id.String()), short(snap.RevisionID))
}

func cmdRebase(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rebase:", err)
		os.Exit(1)
	}
	args := os.Args[2:]
	var positional []string
	var ontoIDs []object.ID
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--onto", "-onto":
			if i+1 < len(args) {
				id, err := object.HexToID(args[i+1])
				if err != nil {
					fmt.Fprintln(os.Stderr, "rebase:", err)
					os.Exit(1)
				}
				ontoIDs = append(ontoIDs, id)
				i++
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 1 || len(ontoIDs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rebase <revision> [--onto <parent-sha>]...")
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)
	ns, ch, err := ws.Rebase(positional[0], ontoIDs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rebase:", err)
		os.Exit(1)
	}
	fmt.Printf("rebased revision %s (id unchanged) -> snapshot %s\n", short(ch.ID), short(ns.RevisionHash.String()))
	// Report conflicts embedded in the new tree, if any.
	atoms, err := ws.ConflictsInTree(ns.TreeID)
	if err == nil && len(atoms) > 0 {
		for _, a := range atoms {
			fmt.Fprintf(os.Stderr, "conflict at %s (resolve <revision> %s)\n", a.Path, a.Path)
		}
	}
}

func cmdResolve(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve:", err)
		os.Exit(1)
	}
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: resolve <revision> <path> [--side N]")
		os.Exit(1)
	}
	changeID := os.Args[2]
	path := os.Args[3]
	side := 0
	for i := 4; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--side", "-side":
			if i+1 < len(os.Args) {
				fmt.Sscanf(os.Args[i+1], "%d", &side)
				i++
			}
		}
	}
	ws := revision.NewWorkspace(repo)
	ns, ch, err := ws.Resolve(changeID, path, side)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve:", err)
		os.Exit(1)
	}
	fmt.Printf("resolved conflict %s in revision %s -> snapshot %s\n", path, short(ch.ID), short(ns.RevisionHash.String()))
}

func cmdSquash(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "squash:", err)
		os.Exit(1)
	}
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: squash <revision>")
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)
	parentSnap, parentCh, err := ws.Squash(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "squash:", err)
		os.Exit(1)
	}
	fmt.Printf("squashed into revision %s -> snapshot %s\n", short(parentCh.ID), short(parentSnap.RevisionHash.String()))
}

func cmdBranch(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "branch:", err)
		os.Exit(1)
	}
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: branch <name> <revision>")
		os.Exit(1)
	}
	r, err := revision.NewWorkspace(repo).SetRef(os.Args[2], store.RefBranch, os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, "branch:", err)
		os.Exit(1)
	}
	fmt.Printf("branch %s -> revision %s\n", r.Name, short(r.Target))
}

func cmdTag(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tag:", err)
		os.Exit(1)
	}
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: tag <name> <revision>")
		os.Exit(1)
	}
	r, err := revision.NewWorkspace(repo).SetRef(os.Args[2], store.RefTag, os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, "tag:", err)
		os.Exit(1)
	}
	fmt.Printf("tag %s -> revision %s\n", r.Name, short(r.Target))
}

func cmdRefs(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "refs:", err)
		os.Exit(1)
	}
	refs, err := revision.NewWorkspace(repo).ListRefs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "refs:", err)
		os.Exit(1)
	}
	for _, r := range refs {
		fmt.Printf("%s  %s -> revision %s\n", r.Kind, r.Name, short(r.Target))
	}
}

func cmdDiff(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "diff:", err)
		os.Exit(1)
	}
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: diff <shaA> <shaB>")
		os.Exit(1)
	}
	a, err := object.HexToID(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "diff:", err)
		os.Exit(1)
	}
	b, err := object.HexToID(os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, "diff:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)
	aTree := snapshotOrTree(ws, a)
	bTree := snapshotOrTree(ws, b)
	fileDiffs, err := ws.DiffContent(aTree, bTree)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diff:", err)
		os.Exit(1)
	}
	for _, fd := range fileDiffs {
		prefix := "M"
		switch fd.Status {
		case revision.StatusAdded:
			prefix = "A"
		case revision.StatusRemoved:
			prefix = "D"
		}
		fmt.Printf("%s %s\n", prefix, fd.Path)
	}
	// Print full unified diffs if content is available (non-empty).
	for _, fd := range fileDiffs {
		if fd.Content != "" {
			fmt.Println("--- " + fd.Path)
			fmt.Println("+++ " + fd.Path)
			fmt.Println(fd.Content)
		}
	}
}

func cmdMerge(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "merge:", err)
		os.Exit(1)
	}
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	var base, ours, theirs string
	fs.StringVar(&base, "base", "", "base tree sha")
	fs.StringVar(&ours, "ours", "", "ours tree sha")
	fs.StringVar(&theirs, "theirs", "", "theirs tree sha")
	fs.Parse(os.Args[2:])
	if base == "" || ours == "" || theirs == "" {
		fmt.Fprintln(os.Stderr, "usage: merge --base <sha> --ours <sha> --theirs <sha>")
		os.Exit(1)
	}
	b, _ := object.HexToID(base)
	o, _ := object.HexToID(ours)
	t, _ := object.HexToID(theirs)
	ws := revision.NewWorkspace(repo)
	mergedID, conflicts, err := ws.Merge(b, o, t)
	if err != nil {
		fmt.Fprintln(os.Stderr, "merge:", err)
		os.Exit(1)
	}
	fmt.Printf("merged tree %s\n", mergedID.String())
	for _, cf := range conflicts {
		fmt.Fprintf(os.Stderr, "conflict at %s\n", cf.Path)
	}
}

// repoTip returns the parent snapshot ids for a fresh fork: the tip of the
// first branch's change if any, otherwise no parents.
func repoTip(repo *store.Repo) []object.ID {
	refs, err := repo.ListRefs()
	if err == nil && len(refs) > 0 {
		for _, r := range refs {
			if r.Kind == store.RefBranch {
				if ch, err := repo.GetRevision(r.Target); err == nil {
					return []object.ID{ch.Hash}
				}
			}
		}
	}
	return nil
}

func snapshotOrTree(ws *revision.Workspace, id object.ID) object.ID {
	if snap, err := ws.GetSnapshot(id); err == nil {
		return snap.TreeID
	}
	return id
}

func splitRepo(s string) (string, string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("expected namespace/name, got %q", s)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func pwd() string { d, _ := filepath.Abs("."); return filepath.Clean(d) }
