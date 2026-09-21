//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal 使用 Linux TTY ioctl 判断标准输入是否适合交互。仅检查字符设备会把
// /dev/null 误判为终端，因此管道和重定向都不能进入读取循环。
func isTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	var settings syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&settings)), 0, 0, 0)
	return errno == 0
}
