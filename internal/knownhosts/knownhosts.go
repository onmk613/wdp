// Package knownhosts 实现 known_hosts 主机指纹账本：加载解析、逐台采集
// 比对（Scan）、坏行自愈与原子重写（Save）。采集（ScanHostKey）与主机段
// 格式化（KnownHostsMarker/KnownHostsLine）一并在此——它们只服务于账本，
// 与连接传输无关（连接侧的主机校验在 conn/sshc，用 x/crypto/knownhosts
// 直接加载）。命令装配（主机来源选择与结果呈现）在 internal/cli。
package knownhosts

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"wdp/internal/model"
)

// Action 是采集到的公钥与现有 known_hosts 条目比对后的处理动作。
type Action int

const (
	ActionExists  Action = iota // 同指纹条目已存在且无过期冲突，无需改动
	ActionAdded                 // 无同主机段条目，追加新行
	ActionUpdated               // 主机段存在旧指纹条目：删除旧行并写入新指纹（或仅清理）
	ActionRevoked               // 采集到的指纹已被 @revoked 吊销，禁止覆盖
)

// ScanResult 是单台主机的采集结果。
type ScanResult struct {
	Host    *model.Host
	KeyType string // 公钥类型（Err 非空时为空）
	Action  Action // 比对动作（Err 非空时无意义）
	Err     error  // 采集失败原因
}

// KnownHosts 代表一个 known_hosts 文件：加载后逐台采集比对（Scan），
// 需要时落盘（Save）。
type KnownHosts struct {
	path    string
	entries []*khEntry
	dirty   bool // 自加载以来条目集有改动（新增/更新/删除），待 Save 落盘
}

// LoadKnownHosts 读取 known_hosts 全部条目；文件不存在时视为空（首次采集）。
func LoadKnownHosts(path string) (*KnownHosts, error) {
	entries, err := readKnownHosts(path)
	if err != nil {
		return nil, err
	}
	kh := &KnownHosts{path: path, entries: entries}
	// 自愈：坏行（严格解析器无法识别的记录）告警并标记删除——Scan 后
	// Save 落盘即清除。一条坏行会让严格加载（连接侧）拒绝整个文件，
	// 留着只会持续炸全部连接；它对校验本身也毫无价值。行内容随告警
	// 打印，便于定位成因。
	for i, e := range entries {
		if e == nil || e.removed {
			continue
		}
		if validKnownHostsLine(e.raw) {
			continue
		}
		fmt.Fprintf(os.Stderr, "[scan-ssh] known_hosts:%d malformed, will be dropped on save: %s\n",
			i+1, truncateLine(e.raw, 100))
		e.removed = true
		kh.dirty = true
	}
	return kh, nil
}

// truncateLine 截断超长行（告警展示用）。
func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Scan 逐台采集公钥并与现有条目比对，结果累积到条目集等待 Save 落盘：
// 仅处理 ssh/push 连接的主机（其余连接类型无主机指纹）。
func (kh *KnownHosts) Scan(hosts []*model.Host) []ScanResult {
	results := make([]ScanResult, 0, len(hosts))
	for _, h := range hosts {
		if h.Conn != "ssh" && h.Conn != "push" {
			continue
		}
		key, err := ScanHostKey(h)
		if err != nil {
			results = append(results, ScanResult{Host: h, Err: err})
			continue
		}
		marker := KnownHostsMarker(h)
		line := KnownHostsLine(h, key)
		action, entries := reconcile(kh.entries, marker, key, line)
		kh.entries = entries
		if action == ActionAdded || action == ActionUpdated {
			kh.dirty = true
		}
		results = append(results, ScanResult{Host: h, KeyType: key.Type(), Action: action})
	}
	return results
}

// Dirty 报告条目集自加载以来是否有待落盘的改动（新增、更新或删除）。
func (kh *KnownHosts) Dirty() bool { return kh.dirty }

// Save 重写 known_hosts（临时文件 + rename 原子替换；权限沿用原文件，新建为 0600）。
func (kh *KnownHosts) Save() error {
	return writeKnownHostsFile(kh.path, kh.entries)
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
		return nil, fmt.Errorf("tcp connection failed: %w", err)
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
		return nil, fmt.Errorf("SSH handshake failed: %w", err)
	}
	return nil, errors.New("no host public key captured")
}

// KnownHostsMarker 返回主机在 known_hosts 中的主机段：非 22 端口或地址含
// 冒号（IPv6 字面量）一律 [host]:port——对齐 OpenSSH put_host_port 行为，
// 否则 IPv6@22 写成裸地址，与校验侧 JoinHostPort 产物 [addr]:22 失配。
func KnownHostsMarker(h *model.Host) string {
	if h.Port != 22 || strings.Contains(h.Address, ":") {
		return fmt.Sprintf("[%s]:%d", h.Address, h.Port)
	}
	return h.Address
}

// KnownHostsLine 生成 known_hosts 行（非 22 端口用 [host]:port 格式）。
func KnownHostsLine(h *model.Host, key ssh.PublicKey) string {
	return knownhosts.Line([]string{KnownHostsMarker(h)}, key)
}

// validKnownHostsLine 严格解析器能否接受该行（自愈用：与连接侧同一判定
// 标准，坏行在 Save 时丢弃）。
func validKnownHostsLine(raw string) bool {
	tmp, err := os.CreateTemp(os.TempDir(), ".known_hosts.chk*")
	if err != nil {
		return true // 无法判定时保守视为有效（不做删除）
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(strings.TrimRight(raw, "\r") + "\n"); err != nil {
		tmp.Close()
		return true
	}
	if err := tmp.Close(); err != nil {
		return true
	}
	_, err = knownhosts.New(tmpName)
	return err == nil
}

// khEntry 是 known_hosts 中的一行：raw 为原文；普通条目附主机模式集与指纹。
type khEntry struct {
	raw     string
	marker  string          // "" 普通条目；"cert-authority"/"revoked" 特殊标记
	hosts   map[string]bool // 主机模式集（已 trim）
	key     ssh.PublicKey
	removed bool
}

// reconcile 将采集到的主机公钥（marker 为 known_hosts 主机段，即 KnownHostsMarker 输出）
// 与现有条目比对，返回动作与更新后的条目列表：
//   - 同 marker 的 @revoked 条目指纹与采集结果一致 → ActionRevoked（吊销是显式安全决策，不覆盖）；
//   - 无同 marker 普通条目 → ActionAdded；
//   - 存在同指纹条目且无过期冲突 → ActionExists；
//   - 存在旧指纹条目 → ActionUpdated：删除全部旧指纹行，若没有同指纹行则追加新行。
//
// 同一 IP 但主机重装/换机导致指纹变化时，旧行被直接删除重写，
// 避免后续连接报 REMOTE HOST IDENTIFICATION HAS CHANGED。
func reconcile(entries []*khEntry, marker string, key ssh.PublicKey, line string) (Action, []*khEntry) {
	for _, e := range entries {
		if e.marker == "revoked" && e.hosts[marker] && sameKey(e.key, key) {
			return ActionRevoked, entries
		}
	}
	same, conflicts := 0, 0
	for _, e := range entries {
		if e.marker != "" || !e.hosts[marker] {
			continue
		}
		if sameKey(e.key, key) {
			same++
		} else {
			conflicts++
		}
	}
	switch {
	case same+conflicts == 0:
		return ActionAdded, append(entries, &khEntry{raw: line, hosts: map[string]bool{marker: true}, key: key})
	case conflicts == 0:
		return ActionExists, entries
	default:
		for _, e := range entries {
			if e.marker == "" && e.hosts[marker] && !sameKey(e.key, key) {
				e.removed = true
			}
		}
		if same == 0 {
			entries = append(entries, &khEntry{raw: line, hosts: map[string]bool{marker: true}, key: key})
		}
		return ActionUpdated, entries
	}
}

// sameKey 比较两个公钥指纹（按序列化字节，避免接口不可比）。
func sameKey(a, b ssh.PublicKey) bool {
	return a != nil && b != nil && bytes.Equal(a.Marshal(), b.Marshal())
}

// parseKHEntry 解析 known_hosts 单行；注释、空白或无法识别的行原样保留且永不参与匹配。
func parseKHEntry(line string) *khEntry {
	m, hosts, pub, _, _, err := ssh.ParseKnownHosts([]byte(line + "\n"))
	if err != nil || len(hosts) == 0 {
		return &khEntry{raw: line}
	}
	set := make(map[string]bool, len(hosts))
	for _, hp := range hosts {
		if hp = strings.TrimSpace(hp); hp != "" {
			set[hp] = true
		}
	}
	return &khEntry{raw: line, marker: m, hosts: set, key: pub}
}

// readKnownHosts 读取 known_hosts 全部行为条目；文件不存在时返回空列表。
func readKnownHosts(path string) ([]*khEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	entries := make([]*khEntry, 0, len(lines))
	for _, line := range lines {
		entries = append(entries, parseKHEntry(line))
	}
	return entries, nil
}

// writeKnownHostsFile 原子重写 known_hosts（临时文件 + rename；权限沿用原文件，新建为 0600）。
func writeKnownHostsFile(path string, entries []*khEntry) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.removed {
			continue
		}
		sb.WriteString(e.raw)
		sb.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(dir, ".known_hosts.tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(sb.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
