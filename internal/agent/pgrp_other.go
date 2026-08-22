//go:build !unix

package agent

import "os/exec"

// 非 Unix 平台无进程组语义，退化为只杀直接子进程。
func setPgrp(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
