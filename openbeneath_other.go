//go:build !linux

package main

import "os"

// openBeneath 在非 Linux 平台上不可用，调用方回落到 Lstat + EvalSymlinks 路径。
func openBeneath(rootFd int, relPath string) (*os.File, bool) { return nil, false }

// openRoot 在非 Linux 平台上不可用。
func openRoot(root string) (*os.File, int) { return nil, -1 }
