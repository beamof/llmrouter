//go:build !windows

package app

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// detachCommand 用新会话运行升级助手/重启后的服务，避免终端关闭时收到 SIGHUP。
func detachCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// prepareUpdateRestart 在 Unix 上无需准备：运行中的可执行文件允许被改名，
// 轮换与重启全部由 finishUpdateRestart 在本进程内完成，不依赖任何子进程。
func prepareUpdateRestart(_, _ string) error { return nil }

// finishUpdateRestart 原地轮换二进制后用 exec 换成新版本继续运行。
// exec 保持同一 PID：systemd 视角服务从未停止；手动运行时进程也留在原终端。
// 任何一步失败都把文件轮换回来，当前进程继续用旧版本服务，不中断。
func finishUpdateRestart(newPath, exe string) {
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[llmrouter-update] "+format+"\n", a...)
	}
	oldPath := exe + ".old"
	os.Remove(oldPath) // 上一次升级残留，先清掉
	if err := os.Rename(exe, oldPath); err != nil {
		logf("无法备份旧程序: %v，取消本次升级（旧程序继续运行）", err)
		os.Remove(newPath)
		return
	}
	if err := os.Rename(newPath, exe); err != nil {
		logf("无法就位新程序: %v，已回滚（旧程序继续运行）", err)
		os.Rename(oldPath, exe)
		return
	}
	logf("新版本已就位，exec 重启 %s", exe)
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		// exec 失败时进程镜像仍是旧版本，把文件轮换回来即可继续服务
		logf("exec 新版本失败: %v，已回滚（旧程序继续运行）", err)
		os.Rename(exe, newPath)
		os.Rename(oldPath, exe)
	}
}

// processAlive 判断进程是否仍在运行；EPERM 表示进程存在但属于其他用户。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
