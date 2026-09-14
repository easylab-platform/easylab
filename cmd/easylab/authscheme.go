package main

import (
	"net/http"
	"strings"
)

// credential extracts the bearer credential from an inbound request,
// accepting every scheme the stack emits. The embedded ops extension sends
// `Authorization: token <t>`; the SDK/gateway send `Bearer <t>`; some
// protocol clients send the raw token. One helper keeps every auth entry
// point (REST labPrincipal, the Connect auth interceptor, the registry)
// consistent.
//
// It returns the credential value and true when a recognizable scheme carried
// a non-empty value; ("", false) otherwise.
func credential(h http.Header) (string, bool) {
	raw := h.Get("Authorization")
	if raw == "" {
		return "", false
	}
	for _, p := range []string{"Bearer ", "bearer ", "token ", "Token "} {
		if v, ok := strings.CutPrefix(raw, p); ok {
			v = strings.TrimSpace(v)
			return v, v != ""
		}
	}
	if strings.ContainsAny(raw, " \t") {
		// An unrecognized scheme ("Basic ...", "Digest ..."): not our
		// credential form.
		return "", false
	}
	// Bare token (cargo registry style: Authorization: <token>).
	return strings.TrimSpace(raw), true
}

// requestCredential is the *http.Request convenience form.
func requestCredential(r *http.Request) (string, bool) { return credential(r.Header) }
