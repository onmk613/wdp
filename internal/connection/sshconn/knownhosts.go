package sshconn

// known_hosts 采集与对账的域逻辑。
// 文件解析、指纹比对、冲突清理、失效记录删除与原子重写集中在此；
// 命令层只负责主机来源选择与结果呈现。

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"wdp/internal/connection"
	"wdp/internal/i18n"
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

// KnownHosts 代表一个 known_hosts 文件：加载后逐台采集比对（Scan）、
// 清理失效主机记录（RemoveHosts），需要时落盘（Save）。
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
	return &KnownHosts{path: path, entries: entries}, nil
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

// RemoveHosts 将主机的全部 known_hosts 条目（普通与 @revoked）标记删除并返回删除行数。
// 用于清理已永久下线主机的残留记录：条目整行删除（与指纹更新的清理策略一致，
// 共享该行的别名主机一并失效）；@cert-authority 是 CA 记录而非主机记录，不随主机删除。
func (kh *KnownHosts) RemoveHosts(hosts []*model.Host) int {
	if len(hosts) == 0 {
		return 0
	}
	markers := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		markers[KnownHostsMarker(h)] = true
	}
	n := 0
	for _, e := range kh.entries {
		if e.marker == "cert-authority" || e.removed {
			continue
		}
		for m := range e.hosts {
			if markers[m] {
				e.removed = true
				n++
				break
			}
		}
	}
	if n > 0 {
		kh.dirty = true
	}
	return n
}

// Save 重写 known_hosts（临时文件 + rename 原子替换；权限沿用原文件，新建为 0600）。
func (kh *KnownHosts) Save() error {
	return writeKnownHostsFile(kh.path, kh.entries)
}

// HostsFromSpecs 解析主机条目列表（host、host:port 或 [ipv6]:port，
// 单个字面 "all" 展开为 known_hosts 中记录的全部主机）为主机列表。
// dc 为组合根注入的连接默认值（可为 nil）。
func HostsFromSpecs(specs []string, khPath string, dc *connection.Defaults) ([]*model.Host, error) {
	if len(specs) == 1 && specs[0] == "all" {
		return hostsFromKnownHosts(khPath, dc)
	}
	out := make([]*model.Host, 0, len(specs))
	for _, spec := range specs {
		h, err := parseHostSpec(spec, dc)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// hostsFromKnownHosts 枚举 known_hosts 普通与 @revoked 条目中的主机模式（去重、按名排序），
// 跳过通配、哈希等无法直接连接的模式；@cert-authority 是 CA 记录而非主机，不参与。
func hostsFromKnownHosts(path string, dc *connection.Defaults) ([]*model.Host, error) {
	entries, err := readKnownHosts(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []*model.Host
	for _, e := range entries {
		if e.marker == "cert-authority" || e.key == nil {
			continue
		}
		for m := range e.hosts {
			if seen[m] || !dialableMarker(m) {
				continue
			}
			h, err := parseHostSpec(m, dc)
			if err != nil {
				continue
			}
			seen[m] = true
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// dialableMarker 判断 known_hosts 主机模式是否可直接连接（排除通配与哈希形式）。
func dialableMarker(m string) bool {
	if m == "" || strings.HasPrefix(m, "|1|") || strings.ContainsAny(m, "*?") {
		return false
	}
	if strings.Contains(m, "[") && !strings.Contains(m, "]:") {
		return false // 字符类通配，如 web[0-9]
	}
	return true
}

// parseHostSpec 解析主机条目：host、host:port 或 [ipv6]:port，缺省端口 22。
// 连接参数（user/超时）取组合根注入的默认值（dc 可为 nil，取内置默认）。
func parseHostSpec(spec string, dc *connection.Defaults) (*model.Host, error) {
	host, port := spec, 22
	if h, p, err := net.SplitHostPort(spec); err == nil {
		n, aerr := strconv.Atoi(p)
		if aerr != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf(i18n.T("host %q has an invalid port: %s", "主机 %q 端口无效: %s"), spec, p)
		}
		host, port = h, n
	} else if strings.Count(spec, ":") > 0 && net.ParseIP(spec) == nil {
		return nil, fmt.Errorf(i18n.T("host %q has an invalid format (expected host, host:port, or [ipv6]:port)", "主机 %q 格式无效（应为 host、host:port 或 [ipv6]:port）"), spec)
	}
	return &model.Host{
		Name:              spec,
		Address:           host,
		Port:              port,
		Conn:              "ssh",
		User:              dc.SSHUserOrDefault(),
		ConnectTimeoutSec: dc.SSHConnectTimeoutOrDefault(),
	}, nil
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
