package sshc

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"wdp/internal/model"
)

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
// 吊销例外：x/crypto 的 @revoked 检查是**全局按密钥字节**的（在主机
// 匹配之前），逐行拆分会让"普通行放行"短路吊销行——因此先收集全部
// @revoked 密钥，组合回调在行匹配之前先查吊销表，保持整文件语义。
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
	var revokedKeys [][]byte // @revoked 行的密钥字节（全局吊销，先于主机匹配检查）
	skipped := 0
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // 空行与注释：本就不参与校验
		}
		if err := rewriteTmp(tmp, line); err != nil {
			return nil, err
		}
		if marker, _, pub, _, _, perr := ssh.ParseKnownHosts([]byte(line + "\n")); perr == nil && marker == "revoked" && pub != nil {
			revokedKeys = append(revokedKeys, pub.Marshal())
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
	return composeHostKeyCallbacks(cbs, revokedKeys), nil
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
// revokedKeys 是 @revoked 行收集的密钥字节：吊销全局生效（与整文件
// knownhosts.New 一致），先于任何行的放行判定。
func composeHostKeyCallbacks(cbs []ssh.HostKeyCallback, revokedKeys [][]byte) ssh.HostKeyCallback {
	isRevoked := func(key ssh.PublicKey) bool {
		kb := key.Marshal()
		for _, r := range revokedKeys {
			if bytes.Equal(kb, r) {
				return true
			}
		}
		return false
	}
	if len(cbs) == 1 && len(revokedKeys) == 0 {
		return cbs[0]
	}
	if len(cbs) == 0 {
		return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
			if key != nil && isRevoked(key) {
				return &knownhosts.RevokedError{} // 吊销优先于"无可用记录"
			}
			return &knownhosts.KeyError{} // 无可用记录：按未知主机上报
		}
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if key != nil && isRevoked(key) {
			return &knownhosts.RevokedError{}
		}
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
