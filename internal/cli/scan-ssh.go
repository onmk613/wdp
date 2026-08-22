package cli

// `wdp scan-ssh`：SSH 主机指纹采集与检查。
// 域逻辑（known_hosts 解析对账、失效记录清理、原子重写、主机条目解析）在
// internal/connection/sshconn；本文件只负责主机来源选择、命令行交互与结果呈现。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"wdp/internal/connection/sshconn"
	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// newScanSshCmd 构造 `wdp scan-ssh`（主机指纹采集与检查）。
func newScanSshCmd() *cobra.Command {
	var (
		knownHosts string
		fromHosts  []string
		prune      bool
	)
	cmd := &cobra.Command{
		Use: "scan-ssh <host pattern>",
		Short: i18n.T("collect host public keys into known_hosts (run once before first connect, since host_key_check defaults on)",
			"采集主机公钥写入 known_hosts（host_key_check 默认开启后首次连接前执行）"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if knownHosts == "" {
				home, _ := os.UserHomeDir()
				knownHosts = filepath.Join(home, ".ssh", "known_hosts")
			}
			if prune && len(fromHosts) == 0 {
				return errors.New(i18n.T("--prune is only available in standalone mode (--from-hosts)",
					"--prune 仅在独立模式（--from-hosts）下可用"))
			}

			// 主机来源：--from-hosts 直接指定时为独立模式，绕过 inventory；
			// 否则用位置参数 <主机模式> 从清单选择。
			var hosts []*model.Host
			if len(fromHosts) > 0 {
				var err error
				hosts, err = sshconn.HostsFromSpecs(fromHosts, knownHosts, connDefaults())
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

			kh, err := sshconn.LoadKnownHosts(knownHosts)
			if err != nil {
				return err
			}
			results := kh.Scan(hosts)

			var deadHosts []*model.Host
			scanned, failed, added, updated := 0, 0, 0, 0
			tb := outPrinter(cmd).NewTable(
				i18n.T("HOST", "主机"), i18n.T("TYPE", "类型"), "FINGERPRINT")
			for _, r := range results {
				if r.Err != nil {
					failed++
					deadHosts = append(deadHosts, r.Host)
					fmt.Fprintf(os.Stderr, "%s: %v\n", r.Host.Name, r.Err)
					if prune {
						tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C("-"),
							fmtutil.CC(i18n.T("pruned (unreachable)", "已删除（无法连接）"), fmtutil.Red))
					}
					continue
				}
				scanned++
				switch r.Action {
				case sshconn.ActionAdded:
					added++
					tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C(r.KeyType),
						fmtutil.CC(i18n.T("added", "新增"), fmtutil.Green))
				case sshconn.ActionUpdated:
					updated++
					tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C(r.KeyType),
						fmtutil.CC(i18n.T("updated", "已更新"), fmtutil.Yellow))
				case sshconn.ActionRevoked:
					failed++
					tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C(r.KeyType),
						fmtutil.CC(i18n.T("revoked (@revoked)", "已吊销（@revoked）"), fmtutil.Red))
					fmt.Fprintf(os.Stderr, "%s: %s\n", r.Host.Name,
						i18n.T("host key is marked @revoked in known_hosts, skipped",
							"主机公钥在 known_hosts 中已被 @revoked 吊销，跳过写入"))
				default:
					tb.AddRow(fmtutil.C(r.Host.Name), fmtutil.C(r.KeyType),
						fmtutil.CC(i18n.T("exists", "已存在"), fmtutil.Dim))
				}
			}
			tb.Render()

			// 独立模式 --prune：连接失败的主机可能已永久下线，
			// 其 known_hosts 记录成为干扰项，按需清理。
			pruned := 0
			if prune && len(deadHosts) > 0 {
				pruned = kh.RemoveHosts(deadHosts)
			}

			if kh.Dirty() {
				if err := kh.Save(); err != nil {
					return fmt.Errorf("写入 known_hosts 失败: %w", err)
				}
			}
			summary := fmt.Sprintf("%s: %d %s（%d %s, %d %s）",
				i18n.T("done", "完成"), scanned, i18n.T("hosts scanned", "台采集"),
				added, i18n.T("added", "新增"), updated, i18n.T("updated", "更新"))
			if pruned > 0 {
				summary += fmt.Sprintf("，%d %s", pruned, i18n.T("stale entries removed", "条失效记录已删除"))
			}
			fmt.Fprintf(out, "%s, known_hosts=%s\n", summary, knownHosts)
			if failed > 0 {
				return fmt.Errorf("%d 台采集失败", failed)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&knownHosts, "known-hosts", "",
		i18n.T("known_hosts path (default ~/.ssh/known_hosts)", "known_hosts 路径（默认 ~/.ssh/known_hosts）"))
	cmd.Flags().StringSliceVar(&fromHosts, "from-hosts", nil,
		i18n.T("standalone mode: check hosts given directly (comma-separated); \"all\" scans every host recorded in known_hosts",
			"独立模式：直接指定要检查的主机（逗号分隔）；指定 all 扫描 known_hosts 中记录的全部主机"))
	cmd.Flags().BoolVar(&prune, "prune", false,
		i18n.T("standalone mode only: remove known_hosts entries of hosts that failed to connect (permanently decommissioned hosts)",
			"仅独立模式：删除连接失败主机的 known_hosts 记录（主机可能已永久下线）"))
	return cmd
}
