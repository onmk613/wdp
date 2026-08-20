package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"wdp/internal/config"
	"wdp/internal/connection/sshconn"
	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// newKeyCmd 构造 `wdp key`（主机指纹管理）。
func newKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: i18n.T("SSH host key management (known_hosts)", "SSH 主机指纹管理（known_hosts）"),
	}
	cmd.AddCommand(newScanCmd())
	return cmd
}

func newScanCmd() *cobra.Command {
	var knownHosts string
	var fromHosts []string
	scan := &cobra.Command{
		Use: "scan <主机模式>",
		Short: i18n.T("collect host public keys into known_hosts (run once before first connect, since host_key_check defaults on)",
			"采集主机公钥写入 known_hosts（host_key_check 默认开启后首次连接前执行）"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if knownHosts == "" {
				home, _ := os.UserHomeDir()
				knownHosts = filepath.Join(home, ".ssh", "known_hosts")
			}

			// 主机来源：--from-hosts 直接指定时绕过 inventory；
			// 否则用位置参数 <主机模式> 从清单选择。
			var hosts []*model.Host
			if len(fromHosts) > 0 {
				var err error
				hosts, err = hostsFromSpecs(fromHosts, knownHosts)
				if err != nil {
					return err
				}
				if len(hosts) == 0 {
					return errors.New(i18n.T("--from-hosts resolved to no scannable hosts",
						"--from-hosts 未解析出可扫描的主机"))
				}
			} else {
				if len(args) == 0 {
					return errors.New(i18n.T("missing host pattern or --from-hosts",
						"缺少 <主机模式> 或 --from-hosts"))
				}
				inv, err := loadInventories()
				if err != nil {
					return err
				}
				hosts, err = inv.Select(args[0])
				if err != nil {
					return err
				}
			}

			entries, err := readKnownHosts(knownHosts)
			if err != nil {
				return err
			}

			scanned, failed, added, updated := 0, 0, 0, 0
			tb := outPrinter(cmd).NewTable(
				i18n.T("HOST", "主机"), i18n.T("TYPE", "类型"), "FINGERPRINT")
			for _, h := range hosts {
				if h.Conn != "ssh" && h.Conn != "push" {
					continue
				}
				key, err := sshconn.ScanHostKey(h)
				if err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "%s: %v\n", h.Name, err)
					continue
				}
				scanned++
				marker := sshconn.KnownHostsMarker(h)
				line := sshconn.KnownHostsLine(h, key)
				action, next := reconcile(entries, marker, key, line)
				entries = next
				switch action {
				case khAdded:
					added++
					tb.AddRow(fmtutil.C(h.Name), fmtutil.C(key.Type()),
						fmtutil.CC(i18n.T("added", "新增"), fmtutil.Green))
				case khUpdated:
					updated++
					tb.AddRow(fmtutil.C(h.Name), fmtutil.C(key.Type()),
						fmtutil.CC(i18n.T("updated", "已更新"), fmtutil.Yellow))
				case khRevoked:
					failed++
					tb.AddRow(fmtutil.C(h.Name), fmtutil.C(key.Type()),
						fmtutil.CC(i18n.T("revoked (@revoked)", "已吊销（@revoked）"), fmtutil.Red))
					fmt.Fprintf(os.Stderr, "%s: %s\n", h.Name,
						i18n.T("host key is marked @revoked in known_hosts, skipped",
							"主机公钥在 known_hosts 中已被 @revoked 吊销，跳过写入"))
				default:
					tb.AddRow(fmtutil.C(h.Name), fmtutil.C(key.Type()),
						fmtutil.CC(i18n.T("exists", "已存在"), fmtutil.Dim))
				}
			}
			tb.Render()

			if added > 0 || updated > 0 {
				if err := writeKnownHostsFile(knownHosts, entries); err != nil {
					return fmt.Errorf("写入 known_hosts 失败: %w", err)
				}
			}
			fmt.Fprintf(out, "%s: %d %s（%d %s, %d %s）, known_hosts=%s\n",
				i18n.T("done", "完成"), scanned, i18n.T("hosts scanned", "台采集"),
				added, i18n.T("added", "新增"),
				updated, i18n.T("updated", "更新"), knownHosts)
			if failed > 0 {
				return fmt.Errorf("%d 台采集失败", failed)
			}
			return nil
		},
	}
	scan.Flags().StringVar(&knownHosts, "known-hosts", "",
		i18n.T("known_hosts path (default ~/.ssh/known_hosts)", "known_hosts 路径（缺省 ~/.ssh/known_hosts）"))
	scan.Flags().StringArrayVar(&fromHosts, "from-hosts", nil,
		i18n.T("scan these hosts instead of an inventory pattern (repeatable; a single 'all' means every host recorded in known_hosts)",
			"直接指定要扫描的主机，绕过 inventory（可多次；仅指定 all 时 = known_hosts 中记录的全部主机）"))

	return scan
}

// hostsFromSpecs 解析 --from-hosts 条目为主机列表；
// 仅指定单个 "all" 时展开为 known_hosts 中记录的全部主机。
func hostsFromSpecs(specs []string, khPath string) ([]*model.Host, error) {
	if len(specs) == 1 && specs[0] == "all" {
		return hostsFromKnownHosts(khPath)
	}
	out := make([]*model.Host, 0, len(specs))
	for _, spec := range specs {
		h, err := parseHostSpec(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// hostsFromKnownHosts 枚举 known_hosts 普通与 @revoked 条目中的主机模式（去重、按名排序），
// 跳过通配、哈希等无法直接连接的模式；@cert-authority 是 CA 记录而非主机，不参与。
func hostsFromKnownHosts(path string) ([]*model.Host, error) {
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
			h, err := parseHostSpec(m)
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

// parseHostSpec 解析 --from-hosts 条目：host、host:port 或 [ipv6]:port，缺省端口 22。
// 连接参数（user/超时）沿用 wdp.cfg 的 [ssh] 配置。
func parseHostSpec(spec string) (*model.Host, error) {
	host, port := spec, 22
	if h, p, err := net.SplitHostPort(spec); err == nil {
		n, aerr := strconv.Atoi(p)
		if aerr != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("主机 %q 端口无效: %s", spec, p)
		}
		host, port = h, n
	} else if strings.Count(spec, ":") > 0 && net.ParseIP(spec) == nil {
		return nil, fmt.Errorf("主机 %q 格式无效（应为 host、host:port 或 [ipv6]:port）", spec)
	}
	cfg := config.Current()
	return &model.Host{
		Name:              spec,
		Address:           host,
		Port:              port,
		Conn:              "ssh",
		User:              cfg.SSHUser(),
		ConnectTimeoutSec: cfg.SSHConnectTimeout(),
	}, nil
}

// khAction 是扫描结果与现有 known_hosts 比对后的处理动作。
type khAction int

const (
	khExists  khAction = iota // 同指纹条目已存在且无过期冲突，无需改动
	khAdded                   // 无同主机段条目，追加新行
	khUpdated                 // 主机段存在旧指纹条目：删除旧行并写入新指纹（或仅清理）
	khRevoked                 // 扫描到的指纹已被 @revoked 吊销，禁止覆盖
)

// khEntry 是 known_hosts 中的一行：raw 为原文；普通条目附主机模式集与指纹。
type khEntry struct {
	raw     string
	marker  string          // "" 普通条目；"cert-authority"/"revoked" 特殊标记
	hosts   map[string]bool // 主机模式集（已 trim）
	key     ssh.PublicKey
	removed bool
}

// reconcile 将扫描到的主机公钥（marker 为 known_hosts 主机段，即 KnownHostsMarker 输出）
// 与现有条目比对，返回动作与更新后的条目列表：
//   - 同 marker 的 @revoked 条目指纹与扫描结果一致 → khRevoked（吊销是显式安全决策，不覆盖）；
//   - 无同 marker 普通条目 → khAdded；
//   - 存在同指纹条目且无过期冲突 → khExists；
//   - 存在旧指纹条目 → khUpdated：删除全部旧指纹行，若没有同指纹行则追加新行。
//
// 同一 IP 但主机重装/换机导致指纹变化时，旧行被直接删除重写，
// 避免后续连接报 REMOTE HOST IDENTIFICATION HAS CHANGED。
func reconcile(entries []*khEntry, marker string, key ssh.PublicKey, line string) (khAction, []*khEntry) {
	for _, e := range entries {
		if e.marker == "revoked" && e.hosts[marker] && sameKey(e.key, key) {
			return khRevoked, entries
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
		return khAdded, append(entries, &khEntry{raw: line, hosts: map[string]bool{marker: true}, key: key})
	case conflicts == 0:
		return khExists, entries
	default:
		for _, e := range entries {
			if e.marker == "" && e.hosts[marker] && !sameKey(e.key, key) {
				e.removed = true
			}
		}
		if same == 0 {
			entries = append(entries, &khEntry{raw: line, hosts: map[string]bool{marker: true}, key: key})
		}
		return khUpdated, entries
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
