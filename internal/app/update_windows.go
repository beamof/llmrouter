//go:build windows

package app

import (
	"os/exec"
	"syscall"
)

// detachCommand 让新进程独立于当前控制台进程组：
// 升级助手/重启后的服务不会随原进程的 Ctrl+C 一起退出，但仍沿用同一控制台输出日志。
func detachCommand(cmd *exec.Cmd) {
	const createNewProcessGroup = 0x00000200
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

// processAlive 判断进程是否仍在运行（仅用于等待旧进程退出，pid 复用不影响正确性：
// 最坏情况是多等几秒后仍会尝试改名）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000 // 标准库 syscall 未导出该常量
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
