//go:build windows

package app

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// prepareUpdateRestart 复制自身到临时目录并启动升级助手：
// Windows 不允许改动运行中的 exe，轮换必须等主进程退出后由助手完成。
func prepareUpdateRestart(newPath, exe string) error {
	helper, err := copySelfToTemp()
	if err != nil {
		return err
	}
	args := append([]string{updateHelperArg, strconv.Itoa(os.Getpid()), newPath, exe}, os.Args[1:]...)
	cmd := exec.Command(helper, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	detachCommand(cmd)
	if err := cmd.Start(); err != nil {
		os.Remove(helper)
		return err
	}
	return nil
}

// finishUpdateRestart 退出主进程，把轮换与重启交给已就位的升级助手。
func finishUpdateRestart(_, _ string) { os.Exit(0) }

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
