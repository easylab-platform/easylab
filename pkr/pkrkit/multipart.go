package pkrkit

import (
	"bytes"
	"strings"
)

// Multipart helpers for adapter uploads. These replicate the byte-exact
// parsing the Go artifact service needed (Swift publish uses
// Content-Transfer-Encoding: binary, which chokes some standard parsers).

// BoundaryFromContentType extracts the boundary parameter of a multipart
// Content-Type, or "" when absent.
func BoundaryFromContentType(ct string) string {
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, "boundary="); ok {
			return strings.Trim(rest, "\"")
		}
	}
	return ""
}

// ExtractField returns the body of the first part whose Content-Disposition
// names `field` (trailing CRLF trimmed).
func ExtractField(body []byte, contentType, field string) ([]byte, bool) {
	boundary := BoundaryFromContentType(contentType)
	if boundary == "" {
		return nil, false
	}
	return extractFieldBoundary(body, boundary, field)
}

func extractFieldBoundary(body []byte, boundary, field string) ([]byte, bool) {
	delim := []byte("--" + boundary)
	rest := body
	for {
		idx := indexOf(rest, delim)
		if idx < 0 {
			return nil, false
		}
		rest = rest[idx+len(delim):]
		// Terminal boundary "--" right after the delimiter.
		if bytes.HasPrefix(rest, []byte("--")) {
			return nil, false
		}
		hdrEnd := indexOf(rest, []byte("\r\n\r\n"))
		if hdrEnd < 0 {
			return nil, false
		}
		headers := rest[:hdrEnd]
		content := rest[hdrEnd+4:]
		next := indexOf(content, delim)
		if next < 0 {
			next = len(content)
		}
		data := content[:next]
		if headerNamesField(headers, field) {
			data = trimCRLF(data)
			return data, true
		}
		if next == 0 {
			return nil, false
		}
		rest = content[next:]
	}
}

// ExtractFirstFile returns (filename, body) of the first file part.
func ExtractFirstFile(body []byte, contentType string) (string, []byte, bool) {
	boundary := BoundaryFromContentType(contentType)
	if boundary == "" {
		return "", nil, false
	}
	delim := []byte("--" + boundary)
	rest := body
	for {
		idx := indexOf(rest, delim)
		if idx < 0 {
			return "", nil, false
		}
		rest = rest[idx+len(delim):]
		if bytes.HasPrefix(rest, []byte("--")) {
			return "", nil, false
		}
		hdrEnd := indexOf(rest, []byte("\r\n\r\n"))
		if hdrEnd < 0 {
			return "", nil, false
		}
		headers := rest[:hdrEnd]
		content := rest[hdrEnd+4:]
		next := indexOf(content, delim)
		if next < 0 {
			next = len(content)
		}
		data := content[:next]
		if fname, ok := filenameFromHeaders(headers); ok {
			return fname, trimCRLF(data), true
		}
		if next == 0 {
			return "", nil, false
		}
		rest = content[next:]
	}
}

// ExtractTextField extracts a simple (non-file) form field, whitespace-trimmed.
func ExtractTextField(body []byte, contentType, field string) (string, bool) {
	data, ok := ExtractField(body, contentType, field)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func headerNamesField(headers []byte, field string) bool {
	if indexOf(headers, []byte(`name="`+field+`"`)) >= 0 {
		return true
	}
	// Some clients omit the quotes.
	return indexOf(headers, []byte("name="+field)) >= 0
}

func filenameFromHeaders(headers []byte) (string, bool) {
	for _, line := range strings.Split(string(headers), "\r\n") {
		if !strings.HasPrefix(strings.ToLower(line), "content-disposition") {
			continue
		}
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if rest, ok := strings.CutPrefix(part, "filename="); ok {
				if f := strings.Trim(rest, "\""); f != "" {
					return f, true
				}
			}
		}
	}
	return "", false
}

func trimCRLF(data []byte) []byte {
	for len(data) > 0 && (data[len(data)-1] == '\n' || data[len(data)-1] == '\r') {
		data = data[:len(data)-1]
	}
	return data
}

func indexOf(h, n []byte) int {
	if len(n) == 0 || len(n) > len(h) {
		return -1
	}
	for i := 0; i+len(n) <= len(h); i++ {
		if bytes.Equal(h[i:i+len(n)], n) {
			return i
		}
	}
	return -1
}
