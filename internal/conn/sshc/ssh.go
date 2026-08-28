// Package sshc 是基于 golang.org/x/crypto/ssh 的连接实现。
// 目标机仅需 POSIX sh（脚本经 base64 传输，规避引号转义问题）；
// 文件传输优先 SFTP，目标机未启用 SFTP 子系统时降级为 exec 流式传输。
package sshc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

func init() {
	conn.RegisterFactory("ssh", func(h *model.Host, dc *conn.Defaults) (conn.Conn, error) {
		return New(h), nil
	})
}

// Conn 是 SSH 连接。
type Conn struct {
	host   *model.Host
	client *ssh.Client
	sftp   *sftp.Client
}

// New 创建 SSH 连接（未建连）。
func New(h *model.Host) *Conn { return &Conn{host: h} }

// Connect 建立 SSH 连接并初始化 SFTP（可用时）。
func (c *Conn) Connect(_ context.Context) error {
	if c.client != nil {
		return nil
	}
	addr := net.JoinHostPort(c.host.Address, fmt.Sprint(c.host.Port))
	timeout := 10 * time.Second
	if c.host.ConnectTimeoutSec > 0 {
		timeout = time.Duration(c.host.ConnectTimeoutSec) * time.Second
	}
	methods, closeAgent, keyWarns := authMethods(c.host)
	cfg := &ssh.ClientConfig{
		User:            c.host.User,
		Auth:            methods,
		HostKeyCallback: hostKeyCallback(c.host),
		Timeout:         timeout,
	}
	// ssh.Dial 不接受 ctx，TCP+握手超时由 cfg.Timeout 控制
	client, err := ssh.Dial("tcp", addr, cfg)
	closeAgent() // 认证在 Dial 内同步完成，agent 的 unix 连接此后不再使用
	if err != nil {
		// 附带私钥解析阶段的诊断：密钥损坏/口令错误时明确指出，
		// 而不是静默滑落到密码认证后被"密码错误"误导
		if len(keyWarns) > 0 {
			return fmt.Errorf("ssh connection failed: %w (private key issues: %s)", err, strings.Join(keyWarns, "; "))
		}
		return fmt.Errorf("ssh connection failed: %w", err)
	}
	c.client = client
	// SFTP 可选（失败时回退 exec 传输）
	if sc, err := sftp.NewClient(c.client); err == nil {
		c.sftp = sc
	}
	return nil
}

// hostKeyCallback 按配置选择指纹校验（known_hosts，默认开启）或显式跳过。
func hostKeyCallback(h *model.Host) ssh.HostKeyCallback {
	if !h.HostKeyCheck {
		return ssh.InsecureIgnoreHostKey()
	}
	path := h.KnownHosts
	if path == "" {
		home, _ := os.UserHomeDir()
		path = home + "/.ssh/known_hosts"
	}
	// 文件不存在时创建空文件（校验仍会拒绝未知主机，但报错信息更明确）
	if _, err := os.Stat(path); os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		f, cerr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if cerr == nil {
			f.Close()
		}
	}
	cb, err := tolerantKnownHosts(path)
	if err != nil {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("known_hosts verification failed (%s): %w", path, err)
		}
	}
	return wrapKeyError(cb)
}

// tolerantKnownHosts 逐行加载 known_hosts：单条坏行跳过并向 stderr 告警
// （对齐 OpenSSH 行为——一条损坏记录不再拖垮全部主机的连接），行尾先剥
// \r（CRLF 行尾文件兼容）。knownhosts 库只提供整文件 New，逐行隔离靠
// 复用一个临时文件轮转喂入实现。返回的组合回调语义与 New 一致：
// 任一行匹配即通过；"指纹不匹配"错误优先于"未知主机"。
func tolerantKnownHosts(path string) (ssh.HostKeyCallback, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".known_hosts.scan*")
	if err != nil {
		// 临时文件不可用时退回严格整文件加载（保持原行为）
		cb, nerr := knownhosts.New(path)
		return cb, nerr
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	var cbs []ssh.HostKeyCallback
	skipped := 0
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // 空行与注释：本就不参与校验
		}
		if err := rewriteTmp(tmp, line); err != nil {
			return nil, err
		}
		cb, err := knownhosts.New(tmpName)
		if err != nil {
			skipped++
			fmt.Fprintf(os.Stderr, "[ssh] known_hosts:%d skipped malformed line: %v\n", i+1, err)
			continue
		}
		cbs = append(cbs, cb)
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "[ssh] known_hosts: %d malformed line(s) skipped (%s)\n", skipped, path)
	}
	return composeHostKeyCallbacks(cbs), nil
}

// rewriteTmp 以单行内容重写临时文件（截断+回写+同步）。
func rewriteTmp(f *os.File, line string) error {
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// ScanHostKey / KnownHostsMarker / KnownHostsLine / known_hosts 账本
// （加载、采集比对、自愈重写）在 internal/knownhosts——它们只服务于
// 指纹采集，与连接传输无关。本包连接侧的主机校验用 x/crypto/knownhosts
// 逐行容错加载（tolerantKnownHosts）。

// composeHostKeyCallbacks 组合多行回调：任一通过即通过；
// 有记录但指纹不匹配（更严重的信号）优先于全部未知主机。
func composeHostKeyCallbacks(cbs []ssh.HostKeyCallback) ssh.HostKeyCallback {
	if len(cbs) == 1 {
		return cbs[0]
	}
	if len(cbs) == 0 {
		return func(hostname string, _ net.Addr, _ ssh.PublicKey) error {
			return &knownhosts.KeyError{} // 无可用记录：按未知主机上报
		}
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		var mismatch error
		for _, cb := range cbs {
			err := cb(hostname, remote, key)
			if err == nil {
				return nil
			}
			if ke, ok := errors.AsType[*knownhosts.KeyError](err); ok && len(ke.Want) > 0 {
				mismatch = err
			}
		}
		if mismatch != nil {
			return mismatch
		}
		return &knownhosts.KeyError{}
	}
}

// wrapKeyError 把标准 known_hosts 错误翻译为带操作指引的提示。
func wrapKeyError(cb ssh.HostKeyCallback) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		if err != nil {
			var ke *knownhosts.KeyError
			if errors.As(err, &ke) && len(ke.Want) == 0 {
				return fmt.Errorf("host %s fingerprint is not in known_hosts (collect the host fingerprint before first connection): %w", hostname, err)
			}
			if errors.As(err, &ke) && len(ke.Want) > 0 {
				return fmt.Errorf("host %s fingerprint does not match known_hosts (possible man-in-the-middle attack; delete the old record and re-collect after verifying): %w", hostname, err)
			}
		}
		return err
	}
}

// Close 关闭连接。
func (c *Conn) Close() error {
	if c.sftp != nil {
		c.sftp.Close()
		c.sftp = nil
	}
	if c.client != nil {
		err := c.client.Close()
		c.client = nil
		return err
	}
	return nil
}

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

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

// UploadFile 上传文件（SFTP 优先，降级 exec）。
func (c *Conn) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	if err := c.ensureClient(); err != nil {
		return err
	}
	if c.sftp != nil {
		if err := mkdirRemote(c.sftp, path.Dir(dst)); err != nil {
			return fmt.Errorf("failed to create remote directory: %w", err)
		}
		tmp := fmt.Sprintf("%s/.wdp.upload.%s", path.Dir(dst), randHex())
		f, err := c.sftp.Create(tmp)
		if err != nil {
			return fmt.Errorf("failed to create remote temp file: %w", err)
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			_ = c.sftp.Remove(tmp)
			return fmt.Errorf("write failed: %w", err)
		}
		if err := f.Chmod(mode.Perm()); err != nil {
			f.Close()
			_ = c.sftp.Remove(tmp)
			return fmt.Errorf("failed to set permissions: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = c.sftp.Remove(tmp) // 关闭失败时清掉远端临时文件
			return err
		}
		if err := c.sftp.PosixRename(tmp, dst); err != nil {
			if err2 := c.sftp.Rename(tmp, dst); err2 != nil {
				_ = c.sftp.Remove(tmp) // 两种改名均失败，清掉临时文件
				return fmt.Errorf("rename failed: %w", err)
			}
		}
		return nil
	}
	// 降级：无 SFTP 时用专用会话流式写入（cat 接 stdin）
	return c.streamUpload(ctx, dst, r, mode)
}

// streamUpload 通过会话 stdin 流式上传：写目标目录下临时文件，成功后
// 原子改名（mv 同目录），失败清理临时文件——不留下部分写入的目标文件。
func (c *Conn) streamUpload(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	sess, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer sess.Close()
	sess.Stdin = r
	var stderr bytes.Buffer
	sess.Stderr = &stderr

	cmd := fmt.Sprintf(`D=%s && mkdir -p -- "$D" && T=$(mktemp "$D/.wdp.upload.XXXXXX") &&
cat > "$T" && chmod %o "$T" && mv -f "$T" %s ||
{ rc=$?; rm -f "$T" 2>/dev/null; exit $rc; }`,
		shellquote.Quote(path.Dir(dst)), mode.Perm(), shellquote.Quote(dst))
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("stream upload failed: %w: %s", err, stderr.String())
		}
		return nil
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return ctx.Err()
	}
}

// DownloadFile 下载文件（SFTP 优先，降级 exec cat）。
func (c *Conn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	if err := c.ensureClient(); err != nil {
		return err
	}
	if c.sftp != nil {
		f, err := c.sftp.Open(src)
		if err != nil {
			return fmt.Errorf("failed to open remote file: %w", err)
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}
	res, err := c.Exec(ctx, conn.ExecRequest{Script: fmt.Sprintf("cat -- %s", shellquote.Quote(src))})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("cat failed: %s", res.Stderr)
	}
	_, err = io.WriteString(w, res.Stdout)
	return err
}

func (c *Conn) ensureClient() error {
	if c.client == nil {
		return errors.New("connection not established")
	}
	return nil
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

// authMethods 组装认证链：身份文件（显式 key_path → ~/.ssh/config 身份
// 文件 → ssh-agent → 默认密钥，每个身份文件自动附带伴随证书
// <私钥>-cert.pub）→ 密码（同时启用 keyboard-interactive）。
// 注意全部公钥签名者必须合并进**单个** ssh.PublicKeys 方法：x/crypto
// 客户端按方法名记录 tried，第一个 publickey 方法失败后其余 publickey
// 方法会被整体跳过——拆成多个方法时"多密钥依次尝试"实际不生效。
// 返回的 cleanup 释放认证过程中打开的资源（agent unix 连接），
// 调用方必须在 ssh.Dial 返回后调用（认证已同步完成）。
// warns 收集"文件存在但解析失败"的私钥诊断（口令错误/密钥损坏），
// 供连接失败时提示根因；文件不存在（如默认密钥未生成）不算异常。
func authMethods(h *model.Host) ([]ssh.AuthMethod, func(), []string) {
	var cleanups []func()
	var warns []string
	cleanup := func() {
		for _, f := range cleanups {
			f()
		}
	}
	passphrase := model.Secret(h.KeyPassphrase, h.KeyPassphraseEnv)

	// parseKeySigners 解析单个身份文件：裸签名者 + 伴随证书签名者
	// （服务器只信任 CA 签发证书、裸公钥不在 authorized_keys 时靠证书认证，
	// 与交互 ssh 自动加载 <私钥>-cert.pub 的行为一致）。
	parseKeySigners := func(p string) []ssh.Signer {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			// 加密私钥：带口令重试
			if pe, ok := err.(*ssh.PassphraseMissingError); ok && pe != nil && passphrase != "" {
				s2, err2 := ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
				if err2 != nil {
					warns = append(warns, fmt.Sprintf("%s: %v", p, err2))
					return nil
				}
				signer = s2
			} else {
				warns = append(warns, fmt.Sprintf("%s: %v", p, err))
				return nil
			}
		}
		signers := []ssh.Signer{signer}
		if cert, warn := certSignerFor(p, signer); cert != nil {
			signers = append(signers, cert)
		} else if warn != "" {
			warns = append(warns, warn)
		}
		return signers
	}

	// 身份文件来源（对齐 OpenSSH）：显式 key_path > ssh config 匹配块的
	// IdentityFile 累积 > 内置默认密钥（配置了 IdentityFile 即不再补默认）。
	// 默认密钥单独返回、由调用方排在 agent 之后，与旧认证链顺序一致。
	keyPaths, defaultKeys := keyFilePaths(h)
	var signers []ssh.Signer
	for _, p := range keyPaths {
		signers = append(signers, parseKeySigners(p)...)
	}

	// ssh-agent（SSH_AUTH_SOCK）：签名者即刻取回并入同一方法（回调形式
	// 是独立的 publickey 方法，签名者非空时才会排在文件密钥后生效，
	// 见函数注释）
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentCl := agent.NewClient(conn)
			if agentSigners, err := agentCl.Signers(); err == nil && len(agentSigners) > 0 {
				signers = append(signers, agentSigners...)
				cleanups = append(cleanups, func() { _ = conn.Close() })
			} else {
				_ = conn.Close() // 无可用 signer，立即释放
			}
		}
	}
	for _, p := range defaultKeys {
		signers = append(signers, parseKeySigners(p)...)
	}

	var methods []ssh.AuthMethod
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	// 密码 + keyboard-interactive
	if pw := model.Secret(h.Password, h.PasswordEnv); pw != "" {
		methods = append(methods,
			ssh.Password(pw),
			ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = pw
				}
				return answers, nil
			}),
		)
	}
	return methods, cleanup, warns
}

// keyFilePaths 归集身份文件来源：primary 为优先尝试的显式/配置文件，
// defaults 为内置默认密钥（primary 为空时才有，由调用方排在 agent 后）。
func keyFilePaths(h *model.Host) (primary, defaults []string) {
	switch {
	case h.KeyPath != "":
		return []string{h.KeyPath}, nil
	case len(h.IdentityFiles) > 0:
		return h.IdentityFiles, nil
	default:
		home, _ := os.UserHomeDir()
		return nil, []string{
			home + "/.ssh/id_ed25519",
			home + "/.ssh/id_ecdsa",
			home + "/.ssh/id_rsa",
		}
	}
}

// certSignerFor 加载身份文件的伴随证书（OpenSSH 约定：<私钥>-cert.pub，
// 已带 .pub 后缀时替换为 -cert.pub）并与私钥组合为证书签名者。
// 证书文件不存在是常态（返回 nil, ""）；存在但无法使用（损坏/与私钥
// 不配对）返回诊断供连接失败时提示根因。
func certSignerFor(keyPath string, signer ssh.Signer) (ssh.Signer, string) {
	certPath := strings.TrimSuffix(keyPath, ".pub") + "-cert.pub"
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, ""
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", certPath, err)
	}
	cert, ok := pub.(*ssh.Certificate)
	if !ok {
		return nil, fmt.Sprintf("%s: not an OpenSSH certificate", certPath)
	}
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", certPath, err)
	}
	return certSigner, ""
}

// mkdirRemote 逐级创建远端目录（已存在则忽略）。
func mkdirRemote(c *sftp.Client, dir string) error {
	if dir == "" || dir == "/" || dir == "." {
		return nil
	}
	if _, err := c.Stat(dir); err == nil {
		return nil
	}
	if err := mkdirRemote(c, path.Dir(dir)); err != nil {
		return err
	}
	if err := c.Mkdir(dir); err != nil && !os.IsExist(err) {
		// sftp 的存在性错误码映射不总是到位，二次探测
		if _, err2 := c.Stat(dir); err2 != nil {
			return err
		}
	}
	return nil
}

// randHex 返回不可预测的 8 字节随机十六进制串（上传临时文件后缀），
// 避免 UnixNano 可预测路径被远端本机用户预创建符号链接劫持。
func randHex() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
