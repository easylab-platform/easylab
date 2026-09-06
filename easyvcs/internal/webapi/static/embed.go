package static

import (
	"embed"
	"io/fs"
)

//go:embed index.html manifest.webmanifest sw.js assets icons
var distFS embed.FS

// FS returns the embedded SPA filesystem rooted at the dist (this dir).
func FS() (fs.FS, error) {
	return fs.Sub(distFS, ".")
}
