//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachCommand 用新会话运行升级助手/重启后的服务，避免终端关闭时收到 SIGHUP。
func detachCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// processAlive 判断进程是否仍在运行；EPERM 表示进程存在但属于其他用户。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
