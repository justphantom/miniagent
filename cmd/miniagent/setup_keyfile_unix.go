//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// maxKeyFileSize 限制 key-file 读取大小（64KiB，API key 远小于此），防误读超大文件
// 或被构造成巨型文件耗资源（审查 P3-5）。多读 1 字节以判定是否越限。
const maxKeyFileSize = 64 << 10

// readKeyFileNoFollow 以 O_NOFOLLOW 拒最终分量 symlink，避免 key-file 被换为指向
// /etc/shadow 的软链致机密外发（与 session openNoFollow 一致）。fd 经 os.NewFile 托管，
// defer f.Close() 与 finalizer 协调（os.File.Close 幂等），杜绝 syscall.Close + finalizer
// 双关同一 fd 的反模式（审查 P3-2）。读量限 64KiB，超限报错（P3-5）。
func readKeyFileNoFollow(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
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
