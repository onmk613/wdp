package sshc

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

// Exec 在远端执行脚本。req.TimeoutMs 为任务级超时（契约同 local/agent
// 通道：0 = 不限），超时终止会话并返回 ctx 错误。
func (c *Conn) Exec(ctx context.Context, req conn.ExecRequest) (conn.ExecResult, error) {
	if err := c.ensureClient(); err != nil {
		return conn.ExecResult{}, err
	}
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	// sudo 密码提权：密码经会话 stdin 首行注入，由包装脚本在同一会话内
	// sudo -S -v 预热凭证后 sudo -n 执行（sudo 凭证缓存按会话隔离，
	// 此前在独立会话预热对实际执行会话无效）
	sudoPW := ""
	if req.BecomeUser != "" {
		sudoPW = model.Secret(c.host.BecomePassword, c.host.BecomePasswordEnv)
	}
	script := WrapScript(req, sudoPW)
	sess, err := c.client.NewSession()
	if err != nil {
		return conn.ExecResult{}, fmt.Errorf("failed to create session: %w", err)
	}
	defer sess.Close()

	switch {
	case sudoPW != "":
		sess.Stdin = strings.NewReader(sudoPW + "\n" + req.Stdin)
	case req.Stdin != "":
		sess.Stdin = strings.NewReader(req.Stdin)
	}
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(script) }()

	select {
	case err := <-done:
		code := 0
		if err != nil {
			code = 1
			if ee, ok := err.(*ssh.ExitError); ok {
				code = ee.ExitStatus()
			}
		}
		return conn.ExecResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return conn.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, ctx.Err()
	}
}

// WrapScript 生成实际下发脚本：env 导出 + base64 解码落盘 + 执行 + 清理。
// sudoPW 非空时（become + 密码提权）：从会话 stdin 首行读取密码并在同一
// 会话内 sudo -S -v 预热凭证（sudo 凭证缓存按会话/ppid 隔离，跨会话不
// 共享），密码不进命令行（ps 不可见），预热后立即 unset。
func WrapScript(req conn.ExecRequest, sudoPW string) string {
	var sb strings.Builder
	for k, v := range req.Env {
		if envKeyRe.MatchString(k) {
			fmt.Fprintf(&sb, "export %s=%s\n", k, shellquote.Quote(v))
		}
	}
	sb.WriteString(req.Script)
	b64 := base64.StdEncoding.EncodeToString([]byte(sb.String()))

	warm := ""
	if sudoPW != "" {
		warm = `IFS= read -r WDP_SUDO_PW || exit 97
printf '%s\n' "$WDP_SUDO_PW" | sudo -S -p '' -v >/dev/null 2>&1 || { unset WDP_SUDO_PW; echo 'wdp: sudo authentication failed' >&2; exit 96; }
unset WDP_SUDO_PW
`
	}
	runner := `sh "$T"`
	setPerm := `chmod 700 "$T"`
	if req.BecomeUser != "" {
		runner = fmt.Sprintf("sudo -n -u %s -- sh \"$T\"", shellquote.Quote(req.BecomeUser))
		// 优先把脚本属主收敛到目标用户后保持 0700（root 登录可 chown，
		// 敏感 env 不暴露给同机其他用户）；无 chown 权限时回退 0644（目标用户需可读）
		setPerm = fmt.Sprintf("chown %s \"$T\" 2>/dev/null && chmod 700 \"$T\" || chmod 644 \"$T\"",
			shellquote.Quote(req.BecomeUser))
	}
	return fmt.Sprintf(`T=$(mktemp /tmp/.wdp.XXXXXX) || exit 99
%sprintf '%%s' '%s' | base64 -d > "$T" || { rm -f "$T"; exit 98; }
%s
%s
rc=$?
rm -f "$T"
exit $rc`, warm, b64, setPerm, runner)
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
