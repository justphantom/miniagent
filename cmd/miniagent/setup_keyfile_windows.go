//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
)

// maxKeyFileSize 限制 key-file 读取大小（64KiB，API key 远小于此），防误读超大文件
// 或被构造成巨型文件耗资源（审查 P3-5）。多读 1 字节以判定是否越限。
const maxKeyFileSize = 64 << 10

// readKeyFileNoFollow 在 Windows 上回退 os.Open：Windows symlink 语义不同（需特权创建，
// 普通用户威胁面小），可接受无 O_NOFOLLOW（审查 P3-4）。读量限 64KiB，超限报错（P3-5）。
func readKeyFileNoFollow(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// 多读 1 字节以判定是否超限：LimitReader 截到 maxKeyFileSize+1，若实得 >maxKeyFileSize
	// 说明源文件越过上限。
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxKeyFileSize {
		return nil, fmt.Errorf("key-file %q 超过 %d 字节上限", path, maxKeyFileSize)
	}
	return data, nil
}
