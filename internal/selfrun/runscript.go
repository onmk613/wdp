package selfrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"wdp/internal/shellquote"
)

// ExecReq 是一次脚本执行请求。
type ExecReq struct {
	Script         string            `json:"script"`
	Stdin          string            `json:"stdin"`
	Env            map[string]string `json:"env"`
	TimeoutMs      int64             `json:"timeout_ms"`
	Cwd            string            `json:"cwd"`
	BecomeUser     string            `json:"become_user"`
	BecomePassword string            `json:"become_password"`
}

// ExecResp 是脚本执行结果。
type ExecResp struct {
	Code      int    `json:"code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	Cancelled bool   `json:"cancelled"` // 调用方取消/断开（区别于超时）
}

// RunScript 在当前进程所在主机上执行一次脚本。
func RunScript(ctx context.Context, req ExecReq) ExecResp {
	// 提权：sudo -u（-n 免密；-S 密码经 stdin 传递，不进命令行，ps 不可见）
	script, stdin := BecomeScript(req.Script, req.BecomeUser, req.BecomePassword, req.Stdin)

	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	// become 时环境变量写进脚本内部（sudo 默认 env_reset 会剥夺外层注入的
	// 变量；脚本内 export 在 sudo 之后执行不受影响），非 become 走进程环境
	env := os.Environ()
	if req.BecomeUser != "" && len(req.Env) > 0 {
		var sb strings.Builder
		for k, v := range req.Env {
			if envKeyRe.MatchString(k) {
				fmt.Fprintf(&sb, "export %s=%s\n", k, shellquote.Quote(v))
			}
		}
		script = sb.String() + script
	} else {
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	setPgrp(cmd) // 独立进程组：超时整组击杀（sudo 提权的 root 子进程不残留）
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Dir = req.Cwd
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	cmd.Env = env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	// 输出上限（每流 1MiB，与控制端截断对齐）：防高输出命令把常驻进程撑爆
	var stdout, stderr capWriter
	stdout.limit, stderr.limit = maxExecOutputBytes, maxExecOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	resp := ExecResp{Stdout: stdout.String(), Stderr: stderr.String()}
	if stdout.truncated {
		resp.Stdout += "\n[wdp-agent] " + "stdout exceeded 1MiB and was truncated"
	}
	if stderr.truncated {
		resp.Stderr += "\n[wdp-agent] " + "stderr exceeded 1MiB and was truncated"
	}
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		resp.Code = -1
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			resp.TimedOut = true
			resp.Stderr += "\n[wdp-agent] " + "execution timed out and was terminated"
		} else {
			// 调用方主动取消/断开不是超时，错误归因不能混为一谈
			resp.Cancelled = true
			resp.Stderr += "\n[wdp-agent] " + "caller cancelled or disconnected"
		}
	} else if err != nil {
		resp.Code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			resp.Code = ee.ExitCode()
		}
	}
	return resp
}
