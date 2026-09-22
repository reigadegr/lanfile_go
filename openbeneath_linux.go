//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// openRoot 打开 root 目录，返回句柄与描述符号，供 openat2 当 dirfd 使用。
// 打开失败（或内核不支持 openat2）时返回 -1，调用方回落到 Lstat + EvalSymlinks 路径。
func openRoot(root string) (*os.File, int) {
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, -1
	}
	file := os.NewFile(uintptr(fd), root)
	if file == nil {
		_ = unix.Close(fd)
		return nil, -1
	}
	return file, fd
}

// openBeneath 用 openat2(2) 打开 rootFd 之内的 relPath：一次系统调用同时完成打开、
// 越界检查与符号链接拒绝，省掉未命中时的 Lstat 与 EvalSymlinks。
//
// RESOLVE_BENEATH 由内核保证解析过程不会越出 root，RESOLVE_NO_SYMLINKS 拒绝路径里的
// 符号链接；路径含符号链接、越界、不存在或内核不支持时返回 false，调用方回落到
// 改动前就有的 Lstat + EvalSymlinks 路径，响应完全一致。
func openBeneath(rootFd int, relPath string) (*os.File, bool) {
	if rootFd < 0 {
		return nil, false
	}
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	fd, err := unix.Openat2(rootFd, relPath, how)
	if err != nil {
		return nil, false
	}
	file := os.NewFile(uintptr(fd), relPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false
	}
	return file, true
}
