package webstatic

import (
	"slices"
	"testing"
)

// Names() is the route-registration source for web.go's mux — its contract (every embedded
// asset, bare names, no directories) is what keeps adding a static file to a one-line change
// (the go:embed directive). Pin it so a future subdirectory embed can't silently leak a
// directory name into the route table.
func TestNames(t *testing.T) {
	names := Names()
	want := []string{"app.css", "app.js", "config.js", "dirpicker.js", "events.js", "index.html", "live.js", "md.js", "panel.js", "store.js", "trajectory.js", "usage.js", "views.js"}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Errorf("Names() = %v, want %v", names, want)
	}
	for _, n := range names {
		if _, err := Read(n); err != nil {
			t.Errorf("Read(%q): %v", n, err)
		}
	}
}
