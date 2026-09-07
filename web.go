package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The frontend is built into frontend/dist (pnpm build) and embedded into the
// binary. A placeholder index.html keeps `go build` working before the first
// frontend build.
//
//go:embed all:frontend/dist
var distFS embed.FS

// spaHandler serves the embedded SPA with a fallback to index.html for
// client-side routes.
func spaHandler() http.Handler {
	sub, err := fs.Sub(distFS, "frontend/dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// Unknown path → SPA route: serve the shell.
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}
