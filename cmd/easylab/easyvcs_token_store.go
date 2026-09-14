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
//
// Ownership boundary: a user IS the ownership boundary. artifactkit's
// Principal.TenantID carries the owning USER id (the field name is legacy).
type easyvcsTokenStore struct {
	cs *store.CentralStore
}

// newEasyvcsTokenStore builds a TokenStore over the easyvcs store.
func newEasyvcsTokenStore(cs *store.CentralStore) artifactkit.TokenStore {
	return &easyvcsTokenStore{cs: cs}
}

// LookupToken resolves a presented token to its principal. easyvcs hashes the
// presented value on lookup (never compares plaintext). Token LEVEL is no
// longer an authorization axis (access is governed by repository roles), so a
// valid token is reported write-capable; the registry ownership layer enforces
// per-name publish rights.
func (s *easyvcsTokenStore) LookupToken(ctx context.Context, token string) (artifactkit.Principal, bool) {
	t, err := s.cs.LookupToken(token)
	if err != nil {
		return artifactkit.Principal{}, false
	}
	u, err := s.cs.GetUser(t.UserID)
	if err != nil || u.Disabled {
		return artifactkit.Principal{}, false
	}
	return artifactkit.Principal{Username: u.Username, Level: artifactkit.LevelWrite, TenantID: u.ID}, true
}

// LookupUsername resolves a principal by username.
func (s *easyvcsTokenStore) LookupUsername(ctx context.Context, username string) (artifactkit.Principal, bool) {
	u, err := s.cs.GetUserByUsername(username)
	if err != nil || u.Disabled {
		return artifactkit.Principal{}, false
	}
	return artifactkit.Principal{Username: username, Level: artifactkit.LevelWrite, TenantID: u.ID}, true
}

// OpenInstance reports whether the deployment has no users at all.
func (s *easyvcsTokenStore) OpenInstance(ctx context.Context) bool {
	return s.cs.IsOpenInstance()
}

// TenantOfToken resolves the owning user id of a credential (token → user).
func (s *easyvcsTokenStore) TenantOfToken(ctx context.Context, token string) int64 {
	t, err := s.cs.LookupToken(token)
	if err != nil {
		return 0
	}
	return t.UserID
}

// TenantOfUsername resolves the owning user id of a username.
func (s *easyvcsTokenStore) TenantOfUsername(ctx context.Context, username string) int64 {
	u, err := s.cs.GetUserByUsername(username)
	if err != nil {
		return 0
	}
	return u.ID
}
