// Package knownhosts 实现 known_hosts 主机指纹账本：加载解析、逐台采集
// 比对（Scan）、坏行自愈与原子重写（Save）。采集（ScanHostKey）与主机段
// 格式化（KnownHostsMarker/KnownHostsLine）一并在此——它们只服务于账本，
// 与连接传输无关（连接侧的主机校验在 conn/sshc，用 x/crypto/knownhosts
// 直接加载）。命令装配（主机来源选择与结果呈现）在 internal/cli。
package knownhosts

import (
	"fmt"
	"os"

	"wdp/internal/model"
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
