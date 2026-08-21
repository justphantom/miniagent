// Package webstatic embeds the -serve WebUI single-page app (vanilla JS, no build step).
// Keeping it a separate package keeps embed.FS out of the main package and mirrors the
// minimonitor layout (static assets beside a tiny assets.go).
package webstatic

import "embed"

//go:embed static/index.html static/app.js static/app.css
var FS embed.FS

// Read returns one embedded static file by name.
func Read(name string) ([]byte, error) {
	return FS.ReadFile("static/" + name)
}
