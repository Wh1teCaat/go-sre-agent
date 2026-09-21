//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package main

import "os"

// isTerminal 在当前未实现 TTY ioctl 的目标系统上保守拒绝默认交互入口；脚本子命令
// 不受影响，避免将重定向输入误判为可交互终端。
func isTerminal(_ *os.File) bool {
	return false
}
