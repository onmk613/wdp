package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"wdp/internal/fmtutil"
	"wdp/internal/knownhosts"

	"github.com/spf13/cobra"
)

const scanSshHelp = `
将主机公钥收集到 known_hosts 文件中

扫描指定主机的 SSH 公钥，并将其添加到 known_hosts 文件中
如果主机公钥已发生变更，除非指定了 --allow-update 参数，否则该命令将拒绝覆盖原有公钥

<host pattern> 指定一个集合和 inventory 中的主机进行匹配多个集合逗号分隔
!符号做为排除前缀，:& 交集链，path.Match 语法做通配匹配主机名和组名

组合示例：
webservers,db-servers            # 两组并集
all,!web1                       # 除 web1 外全部
webservers:&production          # 交集：生产环境的 web
webservers:&production,!canary  # 生产 web 且非金丝雀（docs/03 的例子）
web*                            # 所有 web 开头的组（展开成员）+ 主机
db?                             # db1、dba……单字符
os_[12]                         # os_1、os_2 字符类
prod_*:&app-servers              # 通配与交集链混用
`

// newScanSshCmd 构造 `wdp scan-ssh`（主机指纹采集与检查）。
func newScanSshCmd() *cobra.Command {
	var knownHosts string
	var allowUpdate bool
	cmd := &cobra.Command{
		Use:   "scan-ssh <host-pattern>",
		Short: "collect host public keys into known_hosts",
		Long:  scanSshHelp,
		Args:  cobra.ExactArgs(1),
	}

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()

		inv, err := loadInventories()
		if err != nil {
			return err
		}

		hosts, err := inv.Select(args[0])
		if err != nil {
			return err
		}

		kh, err := knownhosts.LoadKnownHosts(knownHosts)
		if err != nil {
			return err
		}
		results := kh.Scan(hosts)

		scanned, failed, added, updated := 0, 0, 0, 0
		tb := outPrinter(cmd).NewTable("HOST", "TYPE", "FINGERPRINT")
		for _, r := range results {
			if r.Err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "%s: %v\n", r.Host.Name, r.Err)
				continue
			}
			scanned++
			switch r.Action {
			case knownhosts.ActionAdded:
				added++
				tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C(r.KeyType),
					fmtutil.CC("added", fmtutil.Green))
			case knownhosts.ActionUpdated:
				updated++
				tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C(r.KeyType),
					fmtutil.CC("updated", fmtutil.Yellow))
			case knownhosts.ActionRevoked:
				failed++
				tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C(r.KeyType),
					fmtutil.CC("revoked (@revoked)", fmtutil.Red))
				fmt.Fprintf(os.Stderr, "%s: %s\n", r.Host.Name,
					"host key is marked @revoked in known_hosts, skipped")
			default:
				tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C(r.KeyType),
					fmtutil.CC("exists", fmtutil.Dim))
			}
		}
		tb.Render()

		// 指纹变更（ActionUpdated）是安全敏感事件：扫描阶段的网络
		// 中间人可借自动重签永久替换信任锚（运行期连接侧对同一事件
		// 按"可能中间人攻击"拒绝）。此处对齐语义：默认要求逐批确认，
		// --allow-update 显式放行（CI 场景）。
		if updated > 0 && !allowUpdate {
			if !fmtutil.IsTerminal(os.Stdin) || !fmtutil.IsTerminal(os.Stderr) {
				return fmt.Errorf("%d host(s) presented a CHANGED fingerprint; refusing to rewrite known_hosts automatically (possible man-in-the-middle). Verify the change out-of-band, then re-run with --allow-update", updated)
			}
			fmt.Fprintf(os.Stderr, "==> %d host(s) presented a CHANGED fingerprint (possible man-in-the-middle; verify before trusting):\n", updated)
			for _, r := range results {
				if r.Action == knownhosts.ActionUpdated {
					fmt.Fprintf(os.Stderr, "      %s (%s)\n", r.Host.Name, r.KeyType)
				}
			}
			fmt.Fprintf(os.Stderr, "==> overwrite known_hosts fingerprints? [y/N] ")
			line, err := readLine(os.Stdin)
			if err != nil {
				return fmt.Errorf("declined (input error); known_hosts left unchanged")
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y", "yes":
			default:
				return fmt.Errorf("declined; known_hosts left unchanged")
			}
		}

		if kh.Dirty() {
			if err := kh.Save(); err != nil {
				return fmt.Errorf("failed to write known_hosts: %w", err)
			}
		}
		summary := fmt.Sprintf("%s: %d %s(%d %s, %d %s)",
			"done", scanned, "hosts scanned",
			added, "added", updated, "updated")
		fmt.Fprintf(out, "%s, known_hosts=%s\n", summary, knownHosts)
		if failed > 0 {
			return fmt.Errorf("%d host(s) failed to scan", failed)
		}
		return nil
	}

	cmd.Flags().StringVar(&knownHosts, "known-hosts", defaultKnownHostsPath(), "known_hosts path (default ~/.ssh/known_hosts)")
	cmd.Flags().BoolVar(&allowUpdate, "allow-update", false, "overwrite CHANGED host key fingerprints without confirmation")
	return cmd
}

// defaultKnownHostsPath 返回默认 known_hosts 文件路径
func defaultKnownHostsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "known_hosts")
}
