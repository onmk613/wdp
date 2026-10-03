// Package selfrun 是本机脚本执行的单一实现：HTTP /exec 处理器与 agent
// 自治执行连接（conn/selfexec）共用——提权语义（sudo -n / sudo -S 密码经
// stdin 传递）、进程组隔离、超时整组击杀、输出 1MiB 截断只有这一份，
// 不允许两处漂移（docs/15 §7.2：become 静默退化为当前用户执行比失败更危险）。
package selfrun

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// 输出上限的缓冲 writer 收敛至 conn.CapWriter（与 sshc/local 三通道
// 同一实现，防各持一份近似拷贝后口径漂移）；截断标记文案见 execRespOf。

// becomeCmd 构造提权执行的 argv 与 stdin 布局。
// 脚本体不进 argv（本机 ps、/proc/<pid>/cmdline 对同机任意用户可见，脚本
// 内嵌的 export TOKEN=…/口令会广播出去——与 sshc 的 WrapScript 同一威胁
// 模型）：脚本已落盘为 0700 临时文件 path，argv 只引用文件路径。
// 密码经 stdin 首行传递（sudo -S 读首行，余下内容供脚本继续读取），
// 同样不进 argv；req.Stdin 拼接在密码行之后。
// user 直接送入 argv（不经 shell，无注入面），由 sudo 自行解析校验。
func becomeCmd(path, user, password, stdin string) (argv []string, finalStdin string) {
	if password != "" {
		return []string{"sudo", "-S", "-p", "", "-u", user, "--", "/bin/sh", path},
			password + "\n" + stdin
	}
	return []string{"sudo", "-n", "-u", user, "--", "/bin/sh", path}, stdin
}

// handOverTo 把脚本文件归属收敛到 become 目标用户：0700 文件只有属主可读，
// 不收敛属主则目标用户读不了脚本。agent 以 root 常驻时直接 chown 即可；
// 无特权时借 sudo chown（密码经 stdin，不进 argv）；两者都不可用即失败——
// 不静默降级（放宽文件权限会把脚本内嵌的敏感 env 暴露给同机所有用户）。
// sudo 子进程带独立短超时：PAM 提示/LDAP 挂起时父 ctx 可能无 deadline
// （TimeoutMs=0），裸 CombinedOutput 会把整个 RunScript 永久挂住。
func handOverTo(ctx context.Context, path, userName, password string) error {
	u, err := user.Lookup(userName)
	if err != nil {
		return fmt.Errorf("lookup become user %q: %w", userName, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("become user %q has non-numeric uid %q: %w", userName, u.Uid, err)
	}
	if os.Getuid() == uid {
		return nil // 目标就是当前用户：无需换属主
	}
	if err := os.Chown(path, uid, -1); err == nil {
		return nil
	}
	chownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(chownCtx, "sudo", "-n", "-p", "", "--", "chown", u.Uid, path)
	if password != "" {
		cmd = exec.CommandContext(chownCtx, "sudo", "-S", "-p", "", "--", "chown", u.Uid, path)
		cmd.Stdin = strings.NewReader(password + "\n")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cannot hand the script to %s (chown requires root or sudo): %v: %s", userName, err, bytes.TrimSpace(out))
	}
	return nil
}
