package main

import (
	"regexp"
	"strings"
)

// Session names are derived from the workspace triple as `org:repo:branch`
// (e.g. `acme:my.repo:feature-x`). Ported from repo-extension naming.go so
// ops-extension resolves sessions against easylab alone — no other service,
// no mapping table. Components must never contain `:` themselves, which keeps
// the derivation a strict bijection.

var componentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// validComponent reports whether s is acceptable as an org/repo/branch name.
func validComponent(s string) bool {
	if !componentRe.MatchString(s) {
		return false
	}
	if strings.Contains(s, ":") {
		return false // reserved as the separator
	}
	if strings.Contains(s, "..") {
		return false // path traversal / ref rule
	}
	if strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.Contains(s, "//") {
		return false // '//' or leading/trailing '/' would break URL segment round-trips
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return false // git ref rules
	}
	return true
}

// parseSessionName splits a derived session name back into its triple. ok is
// false when the name does not have exactly three `:`-separated valid
// components.
func parseSessionName(name string) (org, repo, branch string, ok bool) {
	parts := strings.Split(name, ":")
	if len(parts) != 3 {
		return "", "", "", false
	}
	for _, p := range parts {
		if !validComponent(p) {
			return "", "", "", false
		}
	}
	return parts[0], parts[1], parts[2], true
}

// tryParseSession is a soft variant of parseSessionName that returns ok=false
// rather than panicking/signing on malformed names (used by service-list).
func tryParseSession(name string) (org, repo, branch string, ok bool) {
	return parseSessionName(name)
}
