package tools

import (
	"reflect"
	"testing"
)

// 无 rtk 的主机上 rtkWrap 必须保留全部原生 argv（曾丢 prefix 导致 `git status` 退化成裸 `git` usage dump）。
// rtkBin 经 PATH 查找且 sync.OnceValue 缓存，注入两个方向：强制无 rtk / 有 rtk。
func TestRtkWrap_NoRtkKeepsFullArgv(t *testing.T) {
	defer withRtkBin(t, "")()
	cases := []struct {
		bin      string
		prefix   []string
		args     []string
		wantBin  string
		wantArgv []string
	}{
		{"git", []string{"git", "-C", "/repo", "--no-pager", "status"}, nil, "git", []string{"-C", "/repo", "--no-pager", "status"}},
		{"git", []string{"git", "-C", "/repo", "--no-pager", "add"}, []string{"f.txt"}, "git", []string{"-C", "/repo", "--no-pager", "add", "f.txt"}},
		{"go", []string{"go", "build"}, []string{"./..."}, "go", []string{"build", "./..."}},
		{"npm", []string{"npm"}, []string{"test"}, "npm", []string{"test"}},
		{"golangci-lint", []string{"golangci-lint", "run"}, []string{"./..."}, "golangci-lint", []string{"run", "./..."}},
	}
	for _, c := range cases {
		bin, argv := rtkWrap(c.bin, c.prefix, c.args)
		if bin != c.wantBin || !reflect.DeepEqual(argv, c.wantArgv) {
			t.Errorf("rtkWrap(%q, %q, %q) = (%q, %q), want (%q, %q)", c.bin, c.prefix, c.args, bin, argv, c.wantBin, c.wantArgv)
		}
	}
}

// withRtkBin overrides the rtkBin cache for the current test (restored via the returned func).
// The sync.OnceValue result cannot be reset, so the whole func variable is swapped.
func withRtkBin(t *testing.T, path string) (restore func()) {
	t.Helper()
	orig := rtkBin
	rtkBin = func() string { return path }
	return func() { rtkBin = orig }
}

func TestRtkWrap_WithRtkRoutesThroughProxy(t *testing.T) {
	defer withRtkBin(t, "/usr/bin/rtk")()
	bin, argv := rtkWrap("git", []string{"git", "-C", "/repo", "status"}, []string{"--short"})
	if bin != "rtk" {
		t.Errorf("bin = %q, want rtk", bin)
	}
	if want := []string{"git", "-C", "/repo", "status", "--short"}; !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}
