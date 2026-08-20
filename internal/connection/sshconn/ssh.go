// Package sshconn 是基于 golang.org/x/crypto/ssh 的连接实现。
// 目标机仅需 POSIX sh（脚本经 base64 传输，规避引号转义问题）；
// 文件传输优先 SFTP，目标机未启用 SFTP 子系统时降级为 exec 流式传输。
package sshconn

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

	"wdp/internal/connection"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

func init() {
	connection.RegisterFactory("ssh", func(h *model.Host) (connection.Connection, error) {
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
func (c *Conn) Connect(ctx context.Context) error {
	if c.client != nil {
		return nil
	}
	addr := net.JoinHostPort(c.host.Address, fmt.Sprint(c.host.Port))
	timeout := 10 * time.Second
	if c.host.ConnectTimeoutSec > 0 {
		timeout = time.Duration(c.host.ConnectTimeoutSec) * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            c.host.User,
		Auth:            authMethods(c.host),
		HostKeyCallback: hostKeyCallback(c.host),
		Timeout:         timeout,
	}
	// ssh.Dial 不接受 ctx，TCP+握手超时由 cfg.Timeout 控制
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return fmt.Errorf("ssh 连接失败: %w", err)
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
	cb, err := knownhosts.New(path)
	if err != nil {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("known_hosts 校验失败（%s）: %w", path, err)
		}
	}
	return wrapKeyError(cb)
}

// wrapKeyError 把标准 known_hosts 错误翻译为带操作指引的提示。
func wrapKeyError(cb ssh.HostKeyCallback) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		if err != nil {
			var ke *knownhosts.KeyError
			if errors.As(err, &ke) && len(ke.Want) == 0 {
				return fmt.Errorf("主机 %s 的指纹不在 known_hosts 中（首次连接请先执行 `wdp key scan` 采集）: %w", hostname, err)
			}
			if errors.As(err, &ke) && len(ke.Want) > 0 {
				return fmt.Errorf("主机 %s 指纹与 known_hosts 记录不一致（疑似中间人攻击；确认无误后删除旧记录再 wdp key scan）: %w", hostname, err)
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

// Exec 在远端执行脚本。
func (c *Conn) Exec(ctx context.Context, req connection.ExecRequest) (connection.ExecResult, error) {
	script := WrapScript(req)
	if err := c.ensureClient(); err != nil {
		return connection.ExecResult{}, err
	}
	// sudo 密码提权：先 sudo -S -v 缓存凭证（每连接的会话时间戳内有效）
	if req.BecomeUser != "" {
		if pw := model.Secret(c.host.BecomePassword, c.host.BecomePasswordEnv); pw != "" {
			if err := c.sudoCache(ctx, pw); err != nil {
				return connection.ExecResult{}, err
			}
		}
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return connection.ExecResult{}, fmt.Errorf("创建会话失败: %w", err)
	}
	defer sess.Close()

	if req.Stdin != "" {
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
		return connection.ExecResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return connection.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, ctx.Err()
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
			return fmt.Errorf("创建远端目录失败: %w", err)
		}
		tmp := fmt.Sprintf("%s/.wdp.upload.%s", path.Dir(dst), randHex())
		f, err := c.sftp.Create(tmp)
		if err != nil {
			return fmt.Errorf("创建远端临时文件失败: %w", err)
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			c.sftp.Remove(tmp)
			return fmt.Errorf("写入失败: %w", err)
		}
		if err := f.Chmod(mode.Perm()); err != nil {
			f.Close()
			c.sftp.Remove(tmp)
			return fmt.Errorf("设置权限失败: %w", err)
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := c.sftp.PosixRename(tmp, dst); err != nil {
			if err2 := c.sftp.Rename(tmp, dst); err2 != nil {
				return fmt.Errorf("改名失败: %w", err)
			}
		}
		return nil
	}
	// 降级：无 SFTP 时用专用会话流式写入（cat 接 stdin）
	return c.streamUpload(ctx, dst, r, mode)
}

// streamUpload 通过会话 stdin 流式上传。
func (c *Conn) streamUpload(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	sess, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("创建会话失败: %w", err)
	}
	defer sess.Close()
	sess.Stdin = r
	var stderr bytes.Buffer
	sess.Stderr = &stderr

	cmd := fmt.Sprintf("mkdir -p -- %s && cat > %s && chmod %o %s",
		shellquote.Quote(path.Dir(dst)), shellquote.Quote(dst), mode.Perm(), shellquote.Quote(dst))
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("流式上传失败: %w: %s", err, stderr.String())
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
			return fmt.Errorf("打开远端文件失败: %w", err)
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}
	res, err := c.Exec(ctx, connection.ExecRequest{Script: fmt.Sprintf("cat -- %s", shellquote.Quote(src))})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("cat 失败: %s", res.Stderr)
	}
	_, err = io.WriteString(w, res.Stdout)
	return err
}

func (c *Conn) ensureClient() error {
	if c.client == nil {
		return fmt.Errorf("连接未建立")
	}
	return nil
}

// sudoCache 通过 sudo -S -v 预热凭证缓存（密码走 stdin）。
func (c *Conn) sudoCache(ctx context.Context, pw string) error {
	sess, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("创建 sudo 会话失败: %w", err)
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(pw + "\n")
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run("sudo -S -p '' -v") }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("sudo 认证失败: %s", strings.TrimSpace(stderr.String()))
		}
		return nil
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return ctx.Err()
	}
}

// WrapScript 生成实际下发脚本：env 导出 + base64 解码落盘 + 执行 + 清理。
func WrapScript(req connection.ExecRequest) string {
	var sb strings.Builder
	for k, v := range req.Env {
		if envKeyRe.MatchString(k) {
			fmt.Fprintf(&sb, "export %s=%s\n", k, shellquote.Quote(v))
		}
	}
	sb.WriteString(req.Script)
	b64 := base64.StdEncoding.EncodeToString([]byte(sb.String()))

	runner := `sh "$T"`
	chmod := "700"
	if req.BecomeUser != "" {
		runner = fmt.Sprintf("sudo -n -u %s -- sh \"$T\"", shellquote.Quote(req.BecomeUser))
		chmod = "644" // 目标用户需可读
	}
	return fmt.Sprintf(`T=$(mktemp /tmp/.wdp.XXXXXX) || exit 99
printf '%%s' '%s' | base64 -d > "$T" || { rm -f "$T"; exit 98; }
chmod %s "$T"
%s
rc=$?
rm -f "$T"
exit $rc`, b64, chmod, runner)
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// authMethods 组装认证链：显式私钥（支持口令）→ ssh-agent → 默认私钥
// → 密码（同时启用 keyboard-interactive）。
func authMethods(h *model.Host) []ssh.AuthMethod {
	var methods []ssh.AuthMethod
	passphrase := model.Secret(h.KeyPassphrase, h.KeyPassphraseEnv)

	parseKey := func(p string) (ssh.AuthMethod, bool) {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, false
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			// 加密私钥：带口令重试
			if pe, ok := err.(*ssh.PassphraseMissingError); ok && pe != nil && passphrase != "" {
				if s2, err2 := ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase)); err2 == nil {
					return ssh.PublicKeys(s2), true
				}
			}
			return nil, false
		}
		return ssh.PublicKeys(signer), true
	}

	var keyPaths []string
	if h.KeyPath != "" {
		keyPaths = []string{h.KeyPath}
	}
	for _, p := range keyPaths {
		if m, ok := parseKey(p); ok {
			methods = append(methods, m)
		}
	}

	// ssh-agent（SSH_AUTH_SOCK）
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentCl := agent.NewClient(conn)
			if signers, err := agentCl.Signers(); err == nil && len(signers) > 0 {
				methods = append(methods, ssh.PublicKeysCallback(agentCl.Signers))
			}
		}
	}

	// 默认私钥
	if h.KeyPath == "" {
		home, _ := os.UserHomeDir()
		for _, p := range []string{
			home + "/.ssh/id_ed25519",
			home + "/.ssh/id_ecdsa",
			home + "/.ssh/id_rsa",
		} {
			if m, ok := parseKey(p); ok {
				methods = append(methods, m)
			}
		}
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
	return methods
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

// ScanHostKey 与目标机完成 SSH 握手并采集主机公钥（认证前阶段，凭据错误不影响采集）。
func ScanHostKey(h *model.Host) (ssh.PublicKey, error) {
	timeout := 10 * time.Second
	if h.ConnectTimeoutSec > 0 {
		timeout = time.Duration(h.ConnectTimeoutSec) * time.Second
	}
	addr := net.JoinHostPort(h.Address, fmt.Sprint(h.Port))
	var captured ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: h.User,
		Auth: []ssh.AuthMethod{ssh.Password("")}, // 占位：指纹在认证前交换
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			captured = key
			return nil
		},
		Timeout: timeout,
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("tcp 连接失败: %w", err)
	}
	defer conn.Close()
	sshc, _, _, err := ssh.NewClientConn(conn, addr, cfg)
	if sshc != nil {
		sshc.Close()
	}
	if captured != nil {
		return captured, nil // 认证失败无所谓，指纹已到手
	}
	if err != nil {
		return nil, fmt.Errorf("SSH 握手失败: %w", err)
	}
	return nil, fmt.Errorf("未采集到主机公钥")
}

// KnownHostsMarker 返回主机在 known_hosts 中的主机段（非 22 端口用 [host]:port 格式）。
func KnownHostsMarker(h *model.Host) string {
	if h.Port != 22 {
		return fmt.Sprintf("[%s]:%d", h.Address, h.Port)
	}
	return h.Address
}

// KnownHostsLine 生成 known_hosts 行（非 22 端口用 [host]:port 格式）。
func KnownHostsLine(h *model.Host, key ssh.PublicKey) string {
	return knownhosts.Line([]string{KnownHostsMarker(h)}, key)
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
