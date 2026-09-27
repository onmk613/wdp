package sshc

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"wdp/internal/model"
)

// hostKeyCallback 按配置选择指纹校验（known_hosts，默认开启）或显式跳过。
// 构建结果经 (path, mtime, size) 包级缓存复用：每次 Connect 都会走到这里，
// 万级主机批量连接时对同一 known_hosts 逐次重解析（读文件 + 临时文件
// 轮转）是纯浪费——文件只在被 ssh-keygen 等工具改写后 mtime/size 才变。
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
	cb, err := cachedTolerantKnownHosts(path)
	if err != nil {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("known_hosts verification failed (%s): %w", path, err)
		}
	}
	return wrapKeyError(cb)
}

// knownHostsCacheEntry 一次成功构建的缓存条目；modTime+size 相同视为
// 文件未变（mtime 粒度内的同尺寸改写理论可漏判，known_hosts 的实际
// 写入方（ssh-keygen）都会同时改变两者，风险可接受）。
type knownHostsCacheEntry struct {
	modTime time.Time
	size    int64
	cb      ssh.HostKeyCallback
}

var (
	knownHostsCacheMu sync.Mutex
	knownHostsCache   = map[string]knownHostsCacheEntry{}
)

// cachedTolerantKnownHosts 带缓存的容错加载：命中即复用回调（回调无状态、
// 可安全共享）；未命中在锁外构建（并发重复构建幂等，后写覆盖）。
func cachedTolerantKnownHosts(path string) (ssh.HostKeyCallback, error) {
	info, err := os.Stat(path)
	if err != nil {
		return tolerantKnownHosts(path) // 保持原错误路径（读失败的具体报错）
	}
	abs := path
	if a, aerr := filepath.Abs(path); aerr == nil {
		abs = a
	}
	knownHostsCacheMu.Lock()
	ent, ok := knownHostsCache[abs]
	knownHostsCacheMu.Unlock()
	if ok && ent.size == info.Size() && ent.modTime.Equal(info.ModTime()) {
		return ent.cb, nil
	}
	cb, err := tolerantKnownHosts(path)
	if err != nil {
		return nil, err
	}
	knownHostsCacheMu.Lock()
	knownHostsCache[abs] = knownHostsCacheEntry{modTime: info.ModTime(), size: info.Size(), cb: cb}
	knownHostsCacheMu.Unlock()
	return cb, nil
}

// tolerantKnownHosts 容错加载 known_hosts：单条坏行跳过并向 stderr 告警
// （对齐 OpenSSH 行为——一条损坏记录不再拖垮全部主机的连接），行尾先剥
// \r（CRLF 行尾文件兼容）。返回回调的语义与整文件 knownhosts.New 一致：
// 任一行匹配即通过、@revoked 全局按密钥字节吊销、"指纹不匹配"错误优先
// 于"未知主机"。
//
// 实现：先试整文件 New（无坏行的常态，零额外开销）；失败才逐行内存
// 预检剔除坏行，好行单次重写（一次 Sync）后一次 New 构建——取代旧
// "每行重写临时文件 + 每行重读整文件"的 O(N²) 轮转。预检用
// ssh.ParseKnownHosts，它不校验行级匹配器（如哈希主机模式），合并后
// New 仍失败时退回逐行隔离加载（perLineKnownHosts，频率极低）。
func tolerantKnownHosts(path string) (ssh.HostKeyCallback, error) {
	if cb, err := knownhosts.New(path); err == nil {
		return cb, nil // 快路径：无坏行，整文件语义即所求
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var good []string
	var revokedKeys [][]byte // @revoked 行的密钥字节（逐行兜底路径的全局吊销表）
	skipped := 0
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // 空行与注释：本就不参与校验
		}
		marker, _, pub, _, _, perr := ssh.ParseKnownHosts([]byte(line + "\n"))
		if perr != nil {
			skipped++
			fmt.Fprintf(os.Stderr, "[ssh] known_hosts:%d skipped malformed line: %v\n", i+1, perr)
			continue
		}
		if marker == "revoked" && pub != nil {
			revokedKeys = append(revokedKeys, pub.Marshal())
		}
		good = append(good, line)
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "[ssh] known_hosts: %d malformed line(s) skipped (%s)\n", skipped, path)
	}
	if len(good) == 0 {
		return composeHostKeyCallbacks(nil, revokedKeys), nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".known_hosts.scan*")
	if err != nil {
		// 临时文件不可用时退回严格整文件加载（保持原行为）
		cb, nerr := knownhosts.New(path)
		return cb, nerr
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	for _, line := range good {
		if _, werr := tmp.WriteString(line + "\n"); werr != nil {
			_ = tmp.Close()
			return nil, werr
		}
	}
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		return nil, serr
	}
	if cerr := tmp.Close(); cerr != nil {
		return nil, cerr
	}
	if cb, nerr := knownhosts.New(tmpName); nerr == nil {
		return cb, nil // 好行合并构建成功：New 自带吊销/不匹配优先语义
	}
	// 兜底：预检放行的行在 New 下仍有个别失败（ParseKnownHosts 不校验的
	// 行级匹配器错误，如损坏的 |1|哈希主机模式）——逐行隔离，单行损失自身
	return perLineKnownHosts(good, revokedKeys, path)
}

// perLineKnownHosts 逐行隔离加载（合并构建失败的兜底，频率极低）：每行
// 单独喂入临时文件构建回调，行内错误只损失该行。revokedKeys 来自预检
// 阶段的收集——逐行拆分会破坏 @revoked 的全局按密钥吊销语义，组合回调
// 在行匹配之前先查吊销表（见 composeHostKeyCallbacks）。
func perLineKnownHosts(lines []string, revokedKeys [][]byte, path string) (ssh.HostKeyCallback, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".known_hosts.scan*")
	if err != nil {
		cb, nerr := knownhosts.New(path)
		return cb, nerr
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	var cbs []ssh.HostKeyCallback
	for i, line := range lines {
		if werr := rewriteTmp(tmp, line); werr != nil {
			return nil, werr
		}
		cb, nerr := knownhosts.New(tmpName)
		if nerr != nil {
			fmt.Fprintf(os.Stderr, "[ssh] known_hosts:%d skipped malformed line: %v\n", i+1, nerr)
			continue
		}
		cbs = append(cbs, cb)
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
