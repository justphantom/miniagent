// Package webstatic embeds the -serve WebUI single-page app (vanilla JS, no build step).
// Keeping it a separate package keeps embed.FS out of the main package and mirrors the
// minimonitor layout (static assets beside a tiny assets.go).
package webstatic

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed static/index.html static/app.css static/app.js static/store.js static/events.js static/md.js static/live.js
var FS embed.FS

// Read returns one embedded static file by name.
func Read(name string) ([]byte, error) {
	return FS.ReadFile("static/" + name)
}

// Names returns the bare file names of every embedded static asset (the "static/" prefix
// stripped). N10: web.go registers routes by iterating this list, so adding a new asset only
// touches the go:embed directive here — the route list is derived, not hand-maintained twice.
func Names() []string {
	names, err := fs.Glob(FS, "static/*")
	if err != nil {
		return nil
	}
	for i, n := range names {
		names[i] = path.Base(n)
	}
	return names
}
