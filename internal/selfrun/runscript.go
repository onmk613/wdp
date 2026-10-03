package selfrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"wdp/internal/conn"
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
	// Label 人读标识（任务名），agent 日志呈现用；不参与执行语义
	Label string `json:"label,omitempty"`
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
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	script, env := prepareScriptEnv(req)
	// 脚本体落 0700 临时文件执行，绝不进 argv：本机 ps、/proc/<pid>/cmdline
	// 对同机任意用户可见，脚本内嵌的 export TOKEN=…/口令会随 argv 广播出去
	// （agent 常以 root 常驻，这是同机横向提权面）——与 sshc.WrapScript 对
	// SSH 通道的防线同一威胁模型，本机通道不能只防其一。
	path, werr := writeScriptFile(script)
	if werr != nil {
		return ExecResp{Code: 1, Stderr: fmt.Sprintf("[wdp-agent] %v", werr)}
	}
	defer os.Remove(path)

	stdin := req.Stdin
	argv := []string{"/bin/sh", path}
	if req.BecomeUser != "" {
		// 0700 文件只有属主可读：先收敛归属再交给 sudo 目标用户执行
		if err := handOverTo(ctx, path, req.BecomeUser, req.BecomePassword); err != nil {
			return ExecResp{Code: 1, Stderr: fmt.Sprintf("[wdp-agent] %v", err)}
		}
		argv, stdin = becomeCmd(path, req.BecomeUser, req.BecomePassword, stdin)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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
	// 输出上限（每流 1MiB，与控制端截断对齐；conn.CapWriter 单一实现）：
	// 防高输出命令把常驻进程撑爆
	var stdout, stderr conn.CapWriter
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return execRespOf(ctx, err, &stdout, &stderr)
}

// prepareScriptEnv 组装最终脚本体与进程环境：become 时环境变量写进脚本
// 内部（sudo 默认 env_reset 会剥夺外层注入的变量；脚本内 export 在 sudo
// 之后执行不受影响），非 become 走进程环境。
func prepareScriptEnv(req ExecReq) (string, []string) {
	script := req.Script
	env := os.Environ()
	if req.BecomeUser != "" && len(req.Env) > 0 {
		var sb strings.Builder
		for k, v := range req.Env {
			if conn.EnvKeyAllowed(k) {
				fmt.Fprintf(&sb, "export %s=%s\n", k, shellquote.Quote(v))
			}
		}
		return sb.String() + script, env
	}
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	return script, env
}

// writeScriptFile 把脚本体落 0700 临时文件（成功后清理归调用方）。
// 任一步失败即删除半成品文件并返回 "<op> script file" 错误（调用方补
// [wdp-agent] 前缀）。
func writeScriptFile(script string) (path string, err error) {
	var tf *os.File
	tf, err = os.CreateTemp("", ".wdp-exec-*.sh")
	if err != nil {
		return "", fmt.Errorf("create script file: %w", err)
	}
	path = tf.Name()
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	if _, werr := tf.WriteString(script); werr != nil {
		tf.Close()
		return "", fmt.Errorf("write script file: %w", werr)
	}
	if werr := tf.Close(); werr != nil {
		return "", fmt.Errorf("close script file: %w", werr)
	}
	if werr := os.Chmod(path, 0o700); werr != nil {
		return "", fmt.Errorf("chmod script file: %w", werr)
	}
	return path, nil
}

// execRespOf 组装执行响应：输出截断标注、退出码归因（超时/调用方取消
// 区分，退出码取真实 exit status）。
func execRespOf(ctx context.Context, err error, stdout, stderr *conn.CapWriter) ExecResp {
	resp := ExecResp{Stdout: stdout.String(), Stderr: stderr.String()}
	if stdout.Truncated() {
		resp.Stdout += "\n[wdp-agent] " + "stdout exceeded 1MiB and was truncated"
	}
	if stderr.Truncated() {
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
