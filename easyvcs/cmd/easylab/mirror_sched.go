package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"easyvcs/internal/mirror"
	"easyvcs/internal/store"
)

// mirrorLoop is the background scheduler. It runs continuously and, on each
// tick, performs two kinds of work:
//
//   - Pull mirror repos: any read-only mirror whose interval has elapsed is
//     refreshed from its external source.
//   - Push mirrors: any normal repo with a push-mirror whose branch tip has
//     moved since the last push is force-pushed to its destinations.
//
// The loop is cheap: it only scans metadata and only does real git work when a
// mirror is due (interval) or dirty (tip changed).
func (s *server) runMirrorLoop(ctx context.Context) {
	enabled := envBool("EASYVCS_MIRROR_ENABLED", true)
	if !enabled {
		return
	}
	tick := envDuration("EASYVCS_MIRROR_TICK", 5*time.Second)
	timer := time.NewTicker(tick)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.mirrorTick(ctx)
		}
	}
}

// mirrorTick performs one scheduled pass. Errors are recorded on the affected
// mirror and logged, never fatal.
func (s *server) mirrorTick(ctx context.Context) {
	s.pullDueMirrors(ctx)
	s.pushDirtyMirrors(ctx)
}

// pullDueMirrors refreshes every read-only mirror whose interval has elapsed.
func (s *server) pullDueMirrors(ctx context.Context) {
	repos, metas, err := s.cs.AllMirrors()
	if err != nil {
		log.Printf("mirror: list mirrors: %v", err)
		return
	}
	now := time.Now().UTC().UnixMilli()
	for _, repo := range repos {
		meta := metas[repo.Namespace+"/"+repo.Name]
		interval := meta.MirrorInterval
		if interval <= 0 {
			continue // manual-only
		}
		intervalMs := int64(interval) * 1000
		if now-meta.MirrorLastSync < intervalMs {
			continue
		}
		if err := s.pullOneMirror(ctx, repo, meta); err != nil {
			log.Printf("mirror: pull %s/%s: %v", repo.Namespace, repo.Name, err)
		}
	}
}

// pushDirtyMirrors force-pushes any push-mirror whose branch tip advanced.
func (s *server) pushDirtyMirrors(ctx context.Context) {
	items, err := s.cs.AllPushMirrors()
	if err != nil {
		log.Printf("mirror: list push mirrors: %v", err)
		return
	}
	for _, item := range items {
		// item.Name is encoded as "ns/repo|name" by AllPushMirrors.
		nsRepo, name := splitPushMirrorName(item.Name)
		repo, err := s.cs.OpenRepo(parseRepoRef(nsRepo))
		if err != nil {
			continue
		}
		if item.LastRev == "" {
			// Never pushed; force-push once.
			s.pushOneMirror(ctx, repo, item, name)
			continue
		}
		ref, err := repo.GetRef(item.Branch)
		if err != nil {
			continue
		}
		if ref.Target == item.LastRev {
			continue // not dirty
		}
		s.pushOneMirror(ctx, repo, item, name)
	}
}

// pushOneMirror runs a single force-push and records the outcome.
func (s *server) pushOneMirror(ctx context.Context, repo *store.Repo, m *store.PushMirror, name string) {
	res, err := mirror.Push(ctx, repo, mirror.PushTarget{
		Name: m.Name, URL: m.URL, Branch: m.Branch, Token: m.Token, LastRev: m.LastRev,
	})
	if err != nil {
		_ = repo.UpdatePushMirrorSync(name, m.LastRev, err.Error())
		log.Printf("mirror: push %s/%s -> %s: %v", repo.Namespace, repo.Name, m.URL, err)
		return
	}
	_ = repo.UpdatePushMirrorSync(name, res.RevID, "")
	log.Printf("mirror: pushed %s/%s (%s) to %s", repo.Namespace, repo.Name, res.RevID, m.URL)
}

// pullOneMirror refreshes a single read-only mirror.
func (s *server) pullOneMirror(ctx context.Context, repo *store.Repo, meta store.RepoMeta) error {
	rev, err := mirror.Pull(ctx, repo, mirror.PullConfig{
		URL: meta.MirrorURL, Branch: meta.MirrorBranch, Token: meta.MirrorToken,
	})
	if err != nil {
		_ = repo.TouchMirrorSync("", time.Now().UTC().UnixMilli(), err.Error())
		return err
	}
	_ = repo.TouchMirrorSync(rev.ID, time.Now().UTC().UnixMilli(), "")
	log.Printf("mirror: pulled %s/%s -> rev %s", repo.Namespace, repo.Name, rev.ID)
	return nil
}

// ---- env helpers ----

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// splitPushMirrorName decodes "ns/repo|name" produced by AllPushMirrors.
func splitPushMirrorName(encoded string) (nsRepo, name string) {
	if i := strings.LastIndex(encoded, "|"); i >= 0 {
		return encoded[:i], encoded[i+1:]
	}
	return encoded, ""
}

// parseRepoRef turns "ns/repo" into a RepoRef.
func parseRepoRef(nsRepo string) store.RepoRef {
	i := strings.Index(nsRepo, "/")
	if i < 0 {
		return store.RepoRef{Namespace: nsRepo}
	}
	return store.RepoRef{Namespace: nsRepo[:i], Name: nsRepo[i+1:]}
}

var _ = fmt.Sprintf
