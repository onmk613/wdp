//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// setPgrp 让子进程成为独立进程组组长：超时/取消时可以杀掉整个进程组，
// 否则只能杀到直接子进程（sh），其派生的工作进程（含 sudo 提权后的
// root 进程）会成为孤儿继续运行。
func setPgrp(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup 杀掉进程组（组长 pid 即组 id）。
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
