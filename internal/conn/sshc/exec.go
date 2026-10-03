package sshc

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
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

	// 脚本体 + 任务 stdin 一起经会话 stdin 投递（不进 argv，见 WrapScript）
	sess.Stdin = strings.NewReader(WrapStdin(req, sudoPW))
	var stdout, stderr conn.CapWriter // 输出上限见 capout.go：防高输出命令打爆控制端
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
		return conn.ExecResult{Code: code, Stdout: capped(&stdout), Stderr: capped(&stderr)}, nil
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return conn.ExecResult{Stdout: capped(&stdout), Stderr: capped(&stderr)}, ctx.Err()
	}
}

// execTo 在远端执行脚本并把 stdout 流式写入 w：不经输出上限缓冲——
// DownloadFile 的 cat 降级路径内容是文件本体而非任务输出，缓冲（OOM）
// 与截断（静默损坏文件、破坏校验和语义）都不可接受，必须直落目标。
func (c *Conn) execTo(ctx context.Context, script string, w io.Writer) error {
	if err := c.ensureClient(); err != nil {
		return err
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer sess.Close()
	sess.Stdout = w
	var stderr conn.CapWriter
	sess.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(script) }()
	select {
	case err := <-done:
		if err != nil {
			if ee, ok := err.(*ssh.ExitError); ok {
				return fmt.Errorf("remote exit %d: %s", ee.ExitStatus(), capped(&stderr))
			}
			return err
		}
		return nil
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return ctx.Err()
	}
}

// WrapScript 生成实际下发脚本：env 导出 + 脚本体落盘 + 执行 + 清理。
//
// 脚本体经 **stdin** 投递（base64 + 哨兵行），绝不进 argv：远端 `sh -c`
// 的实参在整个任务执行期间对同机任意用户可见（ps、/proc/<pid>/cmdline），
// 而脚本通常内嵌 `export TOKEN=…`、数据库口令等敏感值——旧实现把整段
// base64 拼进命令行，等于把这些值广播给同机所有用户。
//
// stdin 布局（按序，由调用方按同一顺序拼接）：
//  1. become 密码行（仅 become 且非免密；sudoPW 非空时由 `sudo -S -v` 读取）
//  2. base64 脚本体，直到哨兵行 payloadSentinel
//  3. 余下字节 = 任务 stdin（原样落 $I，执行时重定向给脚本，字节精确）
//
// 落盘权限：一律 0700。become 到其它用户时用 `sudo -n -- chown` 收敛属主
// （先 chmod 再 chown：chown 之后属主已不是登录用户，再 chmod 会失败）。
// 旧实现在 chown 失败时把脚本放宽到 **0644**，等于把内嵌的口令暴露给
// 同机所有用户；现在失败即失败，不静默降级。
func WrapScript(req conn.ExecRequest, sudoPW string) string {
	warm := ""
	if sudoPW != "" {
		warm = `IFS= read -r WDP_SUDO_PW || exit 97
printf '%s\n' "$WDP_SUDO_PW" | sudo -S -p '' -v >/dev/null 2>&1 || { unset WDP_SUDO_PW; echo 'wdp: sudo authentication failed' >&2; exit 96; }
unset WDP_SUDO_PW
`
	}
	runner := `sh "$T" < "$I"`
	setPerm := `chmod 700 "$T"`
	if req.BecomeUser != "" {
		runner = fmt.Sprintf("sudo -n -u %s -- sh \"$T\" < \"$I\"", shellquote.Quote(req.BecomeUser))
		// 先 chmod（此时属主还是登录用户）再用 sudo 收敛属主；chown 失败
		// 即整条失败——不回落 0644（会泄露脚本内的敏感 env 给同机用户）。
		// 消息文案里的用户名同样经 Quote——裸值嵌在单引号字符串内，含
		// 单引号的 BecomeUser 可闭合引号注入任意命令（chown 参数早已
		// Quote，此处补齐同一口径）。
		setPerm = fmt.Sprintf("chmod 700 \"$T\" && sudo -n -- chown %s \"$T\" || { echo 'wdp: cannot hand the script to %s (passwordless sudo to root required)' >&2; exit 95; }",
			shellquote.Quote(req.BecomeUser), shellquote.Quote(req.BecomeUser))
	}
	return fmt.Sprintf(`T=$(mktemp /tmp/.wdp.XXXXXX) || exit 99
B=$(mktemp /tmp/.wdp.XXXXXX) || exit 99
I=$(mktemp /tmp/.wdp.XXXXXX) || exit 99
trap 'rm -f "$T" "$B" "$I"' EXIT
%s
# 2) 脚本体：base64 直到哨兵行（行式读取不越过哨兵，余下字节留给 3)）
while IFS= read -r WDP_L; do
  [ "$WDP_L" = %s ] && break
  printf '%%s\n' "$WDP_L"
done > "$B" || exit 98
base64 -d < "$B" > "$T" || { echo 'wdp: payload decode failed' >&2; exit 98; }
# 3) 余下 stdin 是任务 stdin（cat 字节精确，不做行处理）
cat > "$I"
%s
%s
rc=$?
rm -f "$T" "$B" "$I"
trap - EXIT
exit $rc`, warm, shellquote.Quote(payloadSentinel), setPerm, runner)
}

// payloadSentinel 是 stdin 中"脚本体结束"的哨兵行。base64 字母表不含下划线，
// 故与合法 payload 不可能撞行。
const payloadSentinel = "__WDP_PAYLOAD_EOF__"

// WrapStdin 构造与 WrapScript 配套的 stdin 流：可选 sudo 密码行 +
// base64 脚本体 + 哨兵行 + 任务 stdin。
func WrapStdin(req conn.ExecRequest, sudoPW string) string {
	var sb strings.Builder
	if sudoPW != "" {
		sb.WriteString(sudoPW)
		sb.WriteString("\n")
	}
	var body strings.Builder
	for k, v := range req.Env {
		if conn.EnvKeyAllowed(k) {
			fmt.Fprintf(&body, "export %s=%s\n", k, shellquote.Quote(v))
		}
	}
	body.WriteString(req.Script)
	sb.WriteString(base64.StdEncoding.EncodeToString([]byte(body.String())))
	sb.WriteString("\n")
	sb.WriteString(payloadSentinel)
	sb.WriteString("\n")
	sb.WriteString(req.Stdin)
	return sb.String()
}
