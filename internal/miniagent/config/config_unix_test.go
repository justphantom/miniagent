//go:build unix

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// ReadFileLimited 经 OpenNoFollow（P2 修复）拒最终分量 symlink，防 config（含 API key）被 symlink 劫持。
// 回归审查盲区：OpenNoFollow 改 ReadFileLimited 的安全增益此前无测试背书。
func TestReadFileLimited_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "realPath.json")
	if err := os.WriteFile(realPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(realPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileLimited(link, 1024); err == nil {
		t.Error("ReadFileLimited 应拒绝 symlink（OpenNoFollow 防 config 劫持）")
	}
	// 对照：真实文件（非 symlink）正常读。
	if _, err := ReadFileLimited(realPath, 1024); err != nil {
		t.Errorf("真实文件应正常读：%v", err)
	}
}
