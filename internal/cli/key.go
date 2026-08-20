package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"wdp/internal/connection/sshconn"
	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
)

// newKeyCmd 构造 `wdp key`（主机指纹管理）。
func newKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: i18n.T("SSH host key management (known_hosts)", "SSH 主机指纹管理（known_hosts）"),
	}
	var knownHosts string
	scan := &cobra.Command{
		Use: "scan <主机模式>",
		Short: i18n.T("collect host public keys into known_hosts (run once before first connect, since host_key_check defaults on)",
			"采集主机公钥写入 known_hosts（host_key_check 默认开启后首次连接前执行）"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inv, err := loadInventories()
			if err != nil {
				return err
			}
			hosts, err := inv.Select(args[0])
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if knownHosts == "" {
				home, _ := os.UserHomeDir()
				knownHosts = filepath.Join(home, ".ssh", "known_hosts")
			}
			existing := map[string]bool{}
			if data, err := os.ReadFile(knownHosts); err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					if line != "" {
						existing[line] = true
					}
				}
			}

			var newLines []string
			scanned, failed := 0, 0
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
				line := sshconn.KnownHostsLine(h, key)
				if existing[line] {
					tb.AddRow(fmtutil.C(h.Name), fmtutil.C(key.Type()),
						fmtutil.CC(i18n.T("exists", "已存在"), fmtutil.Dim))
					continue
				}
				newLines = append(newLines, line)
				tb.AddRow(fmtutil.C(h.Name), fmtutil.C(key.Type()),
					fmtutil.CC(ssh.FingerprintSHA256(key), fmtutil.Green))
			}
			tb.Render()

			if len(newLines) > 0 {
				f, err := os.OpenFile(knownHosts, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					return fmt.Errorf("写入 known_hosts 失败: %w", err)
				}
				for _, l := range newLines {
					fmt.Fprintln(f, l)
				}
				f.Close()
			}
			fmt.Fprintf(out, "%s: %d %s（%d %s）, known_hosts=%s\n",
				i18n.T("done", "完成"), scanned, i18n.T("hosts scanned", "台采集"),
				len(newLines), i18n.T("added", "新增"), knownHosts)
			if failed > 0 {
				return fmt.Errorf("%d 台采集失败", failed)
			}
			return nil
		},
	}
	scan.Flags().StringVar(&knownHosts, "known-hosts", "",
		i18n.T("known_hosts path (default ~/.ssh/known_hosts)", "known_hosts 路径（缺省 ~/.ssh/known_hosts）"))
	cmd.AddCommand(scan)
	return cmd
}
