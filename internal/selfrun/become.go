// Package selfrun 是本机脚本执行的单一实现：HTTP /exec 处理器与 agent
// 自治执行连接（conn/selfexec）共用——提权语义（sudo -n / sudo -S 密码经
// stdin 传递）、进程组隔离、超时整组击杀、输出 1MiB 截断只有这一份，
// 不允许两处漂移（docs/15 §7.2：become 静默退化为当前用户执行比失败更危险）。
package selfrun

import (
	"bytes"
	"fmt"
	"regexp"

	"wdp/internal/shellquote"
)

// envKeyRe 是允许注入的环境变量键白名单（与 sshc 一致）。
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9]*$`)

// maxExecOutputBytes 是单条 exec 每个输出流的缓冲上限（与控制端
// "单任务输出超 1MB 截断"对齐）。
const maxExecOutputBytes = 1 << 20

// capWriter 是带上限的缓冲 writer：保留前 limit 字节，超出部分丢弃并标记。
type capWriter struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.limit = maxExecOutputBytes
	}
	room := w.limit - w.buf.Len()
	if room <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		p = p[:room]
		w.truncated = true
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *capWriter) String() string { return w.buf.String() }

// becomeScript 生成提权执行脚本与最终 stdin 内容。
// 密码经 stdin 传递（sudo -S 读首行，余下内容供 sudo 内命令继续读取），
// 不出现在脚本或命令行 argv 中，防同机用户 ps 窥探；req.Stdin 拼接在密码行之后。
func BecomeScript(script, user, password, stdin string) (finalScript, finalStdin string) {
	if user == "" {
		return script, stdin
	}
	u := shellquote.Quote(user)
	if password != "" {
		return fmt.Sprintf("sudo -S -p '' -u %s -- /bin/sh -c %s", u, shellquote.Quote(script)),
			password + "\n" + stdin
	}
	return fmt.Sprintf("sudo -n -u %s -- /bin/sh -c %s", u, shellquote.Quote(script)), stdin
}
