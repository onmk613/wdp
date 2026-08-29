package knownhosts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Action 是采集到的公钥与现有 known_hosts 条目比对后的处理动作。
type Action int

const (
	ActionExists  Action = iota // 同指纹条目已存在且无过期冲突，无需改动
	ActionAdded                 // 无同主机段条目，追加新行
	ActionUpdated               // 主机段存在旧指纹条目：删除旧行并写入新指纹（或仅清理）
	ActionRevoked               // 采集到的指纹已被 @revoked 吊销，禁止覆盖
)

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
	// 吊销全局生效：@revoked 行按密钥字节匹配，主机段可为具体主机或 *
	// 通配（OpenSSH `@revoked * key` 的标准吊销写法）
	for _, e := range entries {
		if e.marker == "revoked" && (e.hosts[marker] || e.hosts["*"]) && sameKey(e.key, key) {
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
