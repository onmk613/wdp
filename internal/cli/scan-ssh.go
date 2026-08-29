package cli

// `wdp scan-ssh`：SSH 主机指纹采集与检查（服务 inventory 托管的主机：
// 位置参数为主机选择模式，与 run/adhoc 同口径）。
// 域逻辑（known_hosts 解析对账、坏行自愈、原子重写、主机条目解析）在
// internal/knownhosts；本文件只负责主机来源选择、命令行交互与结果呈现。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"wdp/internal/fmtutil"
	"wdp/internal/knownhosts"
)

// newScanSshCmd 构造 `wdp scan-ssh`（主机指纹采集与检查）。
func newScanSshCmd() *cobra.Command {
	var knownHosts string
	var allowUpdate bool
	cmd := &cobra.Command{
		Use:   "scan-ssh <host pattern>",
		Short: "collect host public keys into known_hosts (run once before first connect, since host_key_check defaults on)",

		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if knownHosts == "" {
				home, _ := os.UserHomeDir()
				knownHosts = filepath.Join(home, ".ssh", "known_hosts")
			}
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
			tb := outPrinter(cmd).NewTable(
				"HOST", "TYPE", "FINGERPRINT")
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
		},
	}

	cmd.Flags().StringVar(&knownHosts, "known-hosts", "",
		"known_hosts path (default ~/.ssh/known_hosts)")
	cmd.Flags().BoolVar(&allowUpdate, "allow-update", false,
		"overwrite CHANGED host key fingerprints without confirmation (verify out-of-band first: a changed fingerprint can indicate a man-in-the-middle)")
	return cmd
}
