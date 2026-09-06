package pkrkit

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

// Auth is the protocol-agnostic credential interface. The reference
// implementation maps a static `token=level` table (read/write) onto the
// per-protocol credential models (OCI bearer, cargo raw token, nuget api-key,
// basic auth, ...).
type Auth interface {
	// Authenticate extracts a username from the request, or "" when
	// anonymous. Implementations must understand the common Authorization
	// prefixes plus protocol-specific header forms.
	Authenticate(ctx context.Context, r *http.Request) string

	// CheckBearer validates an OCI bearer token against a scope.
	// wantedScope is like "repository:<name>:<action>".
	CheckBearer(ctx context.Context, token, wantedScope string) (string, bool)

	// CheckToken validates a bare/raw token (cargo, npm publish, ...).
	CheckToken(ctx context.Context, token string) (string, bool)

	// CheckBasic validates Basic credentials.
	CheckBasic(ctx context.Context, user, pass string) bool

	// IssueToken mints an OCI bearer token (returned to clients verbatim).
	IssueToken(ctx context.Context, username string, scopes []string, ttl time.Duration) string
}

// TokenLevel is the privilege carried by a static token.
type TokenLevel string

const (
	LevelRead  TokenLevel = "read"
	LevelWrite TokenLevel = "write"
)

// TokenAuth is the reference Auth implementation for a `token=level` table.
type TokenAuth struct {
	// Tokens maps a token string to its level.
	Tokens map[string]TokenLevel
}

// NewTokenAuth builds a TokenAuth from a `level=token` or `token=level`
// comma/space separated string (e.g. "devtoken=write,readtoken=read").
func NewTokenAuth(spec string) *TokenAuth {
	t := &TokenAuth{Tokens: map[string]TokenLevel{}}
	for _, pair := range splitTokens(spec) {
		parts := splitOnce(pair, '=')
		if len(parts) != 2 {
			continue
		}
		switch TokenLevel(parts[1]) {
		case LevelRead, LevelWrite, "rl", "rw", "r", "w":
			l := TokenLevel(parts[1])
			if l == "rl" || l == "r" {
				l = LevelRead
			}
			if l == "rw" || l == "w" {
				l = LevelWrite
			}
			t.Tokens[parts[0]] = l
		}
	}
	return t
}

func (t *TokenAuth) level(token string) (TokenLevel, bool) {
	l, ok := t.Tokens[token]
	return l, ok
}

func (t *TokenAuth) username(token string) (string, bool) {
	if _, ok := t.level(token); ok {
		return "token:" + token, true
	}
	return "", false
}

func (t *TokenAuth) permits(token, action string) bool {
	if l, ok := t.level(token); ok {
		switch l {
		case LevelWrite:
			return true
		case LevelRead:
			return action == "pull"
		}
	}
	return false
}

// Authenticate implements Auth. It accepts Bearer/token prefixes, a bare
// token, Basic (user:token), and the X-NuGet-ApiKey header.
func (t *TokenAuth) Authenticate(ctx context.Context, r *http.Request) string {
	raw := r.Header.Get("Authorization")
	if prefix, rest, ok := cutPrefix(raw, "Bearer "); ok {
		_ = prefix
		if u, ok := t.username(rest); ok {
			return u
		}
	}
	if prefix, ok := stripTokenPrefix(raw); ok {
		if u, ok := t.username(prefix); ok {
			return u
		}
	}
	// Bare token (cargo registry token sends Authorization: <token>).
	if raw != "" && !hasAnyPrefix(raw, "Bearer ", "token ", "Token ", "Basic ") {
		if u, ok := t.username(raw); ok {
			return u
		}
	}
	if basic, ok := strings.CutPrefix(raw, "Basic "); ok {
		if decoded, err := base64.StdEncoding.DecodeString(basic); err == nil {
			if user, secret, ok := strings.Cut(string(decoded), ":"); ok {
				_ = user
				if u, ok := t.username(strings.TrimSpace(secret)); ok {
					return u
				}
			}
		}
	}
	if key := r.Header.Get("X-NuGet-ApiKey"); key != "" {
		if u, ok := t.username(key); ok {
			return u
		}
	}
	return ""
}

func (t *TokenAuth) CheckBearer(ctx context.Context, token, wantedScope string) (string, bool) {
	_, action, ok := splitAction(wantedScope)
	if !ok {
		return "", false
	}
	if t.permits(token, action) {
		if u, ok := t.username(token); ok {
			return u, true
		}
	}
	return "", false
}

func (t *TokenAuth) CheckToken(ctx context.Context, token string) (string, bool) {
	return t.username(token)
}

func (t *TokenAuth) CheckBasic(ctx context.Context, _, pass string) bool {
	_, ok := t.level(pass)
	return ok
}

func (t *TokenAuth) IssueToken(ctx context.Context, username string, scopes []string, ttl time.Duration) string {
	// Static tokens ARE the credentials: return the write token verbatim so
	// it round-trips as a Bearer credential.
	for tok, l := range t.Tokens {
		if l == LevelWrite {
			return tok
		}
	}
	return ""
}

// --- small helpers (kept internal to avoid depending on stdlib extras) -------

func splitTokens(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ',' || c == ' ' || c == '\n' || c == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func splitOnce(s string, sep byte) [2]string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return [2]string{s[:i], s[i+1:]}
		}
	}
	return [2]string{""}
}

func cutPrefix(s, prefix string) (string, string, bool) {
	if strings.HasPrefix(s, prefix) {
		return prefix, s[len(prefix):], true
	}
	return "", s, false
}

func stripTokenPrefix(raw string) (string, bool) {
	for _, p := range []string{"Bearer ", "token ", "Token "} {
		if strings.HasPrefix(raw, p) {
			return raw[len(p):], true
		}
	}
	return "", false
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func splitAction(scope string) (repo string, action string, ok bool) {
	// wantedScope: "repository:<name>:<action>".
	for i := len(scope) - 1; i >= 0; i-- {
		if scope[i] == ':' {
			return scope[:i], scope[i+1:], true
		}
	}
	return "", "", false
}
