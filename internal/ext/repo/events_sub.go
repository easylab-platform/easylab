package repoext

import (
	"context"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
)

// handleLifecycleEvent mirrors one agent lifecycle event into the workspace
// layer. Every step is idempotent: redeliveries (at-least-once) converge.
func (s *server) handleLifecycleEvent(ctx context.Context, event string, env abcprotocol.LifecycleEvent, tenant string) error {
	var err error
	switch event {
	case "created":
		org, repo, bm, ok := parseSession(env.SessionName)
		if !ok {
			return errBad("session %q does not match org:repo:branch naming — ignoring", env.SessionName)
		}
		err = s.ensureCreated(ctx, org, repo, bm, env.SessionName)
	case "forked":
		org, repo, bm, ok := parseSession(env.SessionName)
		if !ok {
			return errBad("session %q does not match org:repo:branch naming — ignoring", env.SessionName)
		}
		err = s.ensureForked(ctx, org, repo, bm, env.SessionName, strp(env.Parent))
	case "renamed":
		err = s.ensureRenamed(ctx, strp(env.From), strp(env.To))
	case "deleted":
		err = s.ensureDeleted(ctx, env.SessionName)
	default:
		return errBad("unknown lifecycle event %q — ignoring", event)
	}
	if err != nil {
		return err
	}

	// Project the workspace mapping into the shared var KV (single-source-of-
	// truth = our PG; KV = recomputable projection). `deleted` clears instead.
	if event == "deleted" {
		s.clearSessionVars(ctx, s.ext, tenant, env.SessionName)
		return nil
	}
	sid := env.SessionName
	if event == "forked" || event == "renamed" {
		if t := strp(env.To); t != "" {
			sid = t
		}
	}
	s.publishSessionVars(ctx, s.ext, tenant, sid)
	return nil
}

// ensureCreated: repo + branch from `main` (head fallback) + mapping row.
func (s *server) ensureCreated(ctx context.Context, org, repo, bm, sid string) error {
	if row, err := s.store.GetRowBySession(ctx, sid); err != nil {
		return errDownstream("postgres", err)
	} else if row != nil {
		return nil // already mirrored
	}
	if err := s.lab.EnsureRepo(ctx, org, repo); err != nil {
		return err
	}
	if err := s.ensureBranchAnchored(ctx, org, repo, bm); err != nil {
		return err
	}
	return s.bindRow(ctx, org, repo, bm, sid)
}

// ensureForked: branch from the parent's branch (true workspace
// inheritance at fork time) + mapping row. The parent is materialized first
// when its own event was missed (e.g. pre-event sessions).
func (s *server) ensureForked(ctx context.Context, org, repo, bm, sid, parentSid string) error {
	if row, err := s.store.GetRowBySession(ctx, sid); err != nil {
		return errDownstream("postgres", err)
	} else if row != nil {
		return nil
	}

	// Resolve the parent's branch; materialize the parent when unmapped
	// (legacy or missed event) so the fork anchors at real work.
	var parentBM string
	if pOrg, pRepo, pBM, ok := parseSession(parentSid); ok {
		if pOrg != org || pRepo != repo {
			return errBad("fork across repositories (%s → %s/%s) — unsupported", parentSid, org, repo)
		}
		parentBM = pBM
		if prow, err := s.store.GetRow(ctx, org, repo, pBM); err != nil {
			return errDownstream("postgres", err)
		} else if prow == nil {
			if err := s.ensureCreated(ctx, org, repo, pBM, parentSid); err != nil {
				return err
			}
		}
	} else {
		parentBM = "main" // non-derived parent: anchor at main
	}

	if err := s.lab.EnsureBranch(ctx, org, repo, parentBM, bm); err != nil {
		return err
	}
	return s.bindRow(ctx, org, repo, bm, sid)
}

// ensureRenamed: dual rename — new branch at the old one's position, row
// update, old branch removed.
func (s *server) ensureRenamed(ctx context.Context, fromSid, toSid string) error {
	fromOrg, fromRepo, fromBM, ok := parseSession(fromSid)
	if !ok {
		return errBad("session %q does not match naming — ignoring rename", fromSid)
	}
	toOrg, toRepo, toBM, ok := parseSession(toSid)
	if !ok {
		return errBad("session %q does not match naming — ignoring rename", toSid)
	}
	if fromOrg != toOrg || fromRepo != toRepo {
		return errBad("rename across repositories (%s → %s) — unsupported", fromSid, toSid)
	}

	row, err := s.store.GetRow(ctx, fromOrg, fromRepo, fromBM)
	if err != nil {
		return errDownstream("postgres", err)
	}
	if row == nil {
		// Old name never had a workspace; treat as a plain create.
		return s.ensureCreated(ctx, toOrg, toRepo, toBM, toSid)
	}
	if err := s.lab.EnsureBranch(ctx, fromOrg, fromRepo, fromBM, toBM); err != nil {
		return err
	}
	if err := s.store.RenameRow(ctx, fromOrg, fromRepo, fromBM, toBM, toSid); err != nil {
		if statusOf(err) == 409 {
			return nil // concurrent rename already won
		}
		return errDownstream("postgres", err)
	}
	s.cache.evict(fromSid)
	return s.lab.DeleteBranch(ctx, fromOrg, fromRepo, fromBM)
}

// ensureDeleted: branch + mapping row removed (branch-first order: a
// crash mid-way leaves an adoptable orphan, never a dangling row).
func (s *server) ensureDeleted(ctx context.Context, sid string) error {
	org, repo, bm, ok := parseSession(sid)
	if !ok {
		return errBad("session %q does not match naming — ignoring delete", sid)
	}
	row, err := s.store.GetRowBySession(ctx, sid)
	if err != nil {
		return errDownstream("postgres", err)
	}
	if row == nil {
		return nil
	}
	if err := s.lab.DeleteBranch(ctx, org, repo, bm); err != nil {
		return err
	}
	if err := s.store.DeleteRow(ctx, org, repo, bm); err != nil {
		return errDownstream("postgres", err)
	}
	s.cache.evict(sid)
	return nil
}

// ensureBranchAnchored creates branch at `main`, falling back to the
// repo head when `main` itself does not exist (fresh repo bootstrap order).
func (s *server) ensureBranchAnchored(ctx context.Context, org, repo, bm string) error {
	anchor := "main"
	if ok, err := s.lab.CanResolve(ctx, org, repo, anchor); err != nil {
		return err
	} else if !ok {
		anchor = "" // easylab resolves "" as the repo head
	}
	return s.lab.EnsureBranch(ctx, org, repo, anchor, bm)
}

// strp safely derefs the optional lifecycle fields (parent/from/to).
func strp(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
