package main

import (
	"context"

	artifactkit "github.com/easylab-platform/artifact/core"
	"github.com/easylab-platform/easyvcs/store"
)

// easyvcsTokenStore adapts the easyvcs CentralStore credential tables
// (users + tokens, SHA-256 at rest) onto artifactkit's TokenStore seam. It
// lets the package registry authenticate against the same store the Lab uses,
// without artifact adopting easyvcs storage.
type easyvcsTokenStore struct {
	cs *store.CentralStore
}

// newEasyvcsTokenStore builds a TokenStore over the easyvcs store.
func newEasyvcsTokenStore(cs *store.CentralStore) artifactkit.TokenStore {
	return &easyvcsTokenStore{cs: cs}
}

// LookupToken resolves a presented token to its principal. easyvcs hashes the
// presented value on lookup (never compares plaintext).
func (s *easyvcsTokenStore) LookupToken(ctx context.Context, token string) (artifactkit.Principal, bool) {
	t, err := s.cs.LookupToken(token)
	if err != nil {
		return artifactkit.Principal{}, false
	}
	return artifactkit.Principal{Username: tokenUsername(s.cs, t.UserID), Level: tokenLevel(t.Level)}, true
}

// LookupUsername resolves a principal by username, reporting the strongest
// credential level registered for that user.
func (s *easyvcsTokenStore) LookupUsername(ctx context.Context, username string) (artifactkit.Principal, bool) {
	u, err := s.cs.GetUserByUsername(username)
	if err != nil {
		return artifactkit.Principal{}, false
	}
	toks, err := s.cs.ListTokens(u.ID)
	if err != nil || len(toks) == 0 {
		// A user with no credential row cannot obtain write: default to the
		// read (pull-only) level so a mint stays fail-closed.
		return artifactkit.Principal{Username: username, Level: artifactkit.LevelRead}, true
	}
	level := artifactkit.LevelRead
	for _, t := range toks {
		if t.Level == "write" || t.Level == "admin" {
			level = artifactkit.LevelWrite
			break
		}
	}
	return artifactkit.Principal{Username: username, Level: level}, true
}

// OpenInstance reports whether the deployment has no users at all.
func (s *easyvcsTokenStore) OpenInstance(ctx context.Context) bool {
	return s.cs.IsOpenInstance()
}

func tokenLevel(level string) artifactkit.TokenLevel {
	if level == "write" || level == "admin" {
		return artifactkit.LevelWrite
	}
	return artifactkit.LevelRead
}

func tokenUsername(cs *store.CentralStore, userID int64) string {
	if u, err := cs.GetUser(userID); err == nil {
		return u.Username
	}
	return ""
}
