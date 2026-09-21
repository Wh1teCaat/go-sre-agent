//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal 使用 BSD 系统的 TTY ioctl 判断标准输入是否适合交互，避免将 /dev/null
// 等字符设备误判为终端。
func isTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	var settings syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&settings)))
	return errno == 0
}
