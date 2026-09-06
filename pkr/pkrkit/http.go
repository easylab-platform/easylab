package pkrkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// JSON writes an application/json response with the given body value.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Text writes a response with an explicit content type.
func Text(w http.ResponseWriter, status int, body, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// Error writes an {"ok":false,"error":msg} JSON error response.
func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]any{"ok": false, "error": msg})
}

// BlobResponse writes an application/octet-stream body with a content-disposition.
func BlobResponse(w http.ResponseWriter, data []byte, filename string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// OctetResponse writes an application/octet-stream body without a filename.
func OctetResponse(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// URLencode percent-encodes a path segment (RFC 3986 unreserved set plus ~
// stay literal, everything else is %XX uppercase).
func URLencode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// AuthorizeWrite gates a write operation on an optional Auth. On success it
// returns ""; on failure it writes a 401 challenge and returns a non-empty
// marker (the caller should stop).
func AuthorizeWrite(w http.ResponseWriter, r *http.Request, auth Auth) bool {
	if auth == nil {
		return true
	}
	if auth.Authenticate(r.Context(), r) == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		JSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "authentication required"})
		return false
	}
	return true
}
