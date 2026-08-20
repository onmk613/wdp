package cli

// 安全与信任命令组（--help 的 Security & Trust Commands）：自建 CA 与证书签发。

import (
	"fmt"

	"github.com/spf13/cobra"

	"wdp/internal/ca"
	"wdp/internal/i18n"
)

// newCACmd 构造 `wdp ca`（mTLS 证书工具）。
func newCACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: i18n.T("self-managed CA and certificate issuing (for agent mTLS)", "自建 CA 与证书签发（agent mTLS 认证用）"),
	}
	var (
		initDir, initPass string
	)
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "初始化 CA（ca.crt / ca.key，--passphrase 加密私钥）",
		RunE: func(cmd *cobra.Command, args []string) error {
			crt, key, fp, err := ca.Init(initDir, initPass)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, crt)
			fmt.Fprintln(out, key)
			fmt.Fprintf(out, "%s: %s\n", i18n.T("CA fingerprint", "CA 指纹"), fp)
			return nil
		},
	}
	initCmd.Flags().StringVarP(&initDir, "dir", "d", ".", i18n.T("CA directory", "CA 目录"))
	initCmd.Flags().StringVar(&initPass, "passphrase", "",
		i18n.T("CA private key passphrase (default from "+ca.PassphraseEnv+" env; empty = stored in plaintext)",
			"CA 私钥口令（缺省取 "+ca.PassphraseEnv+" 环境变量；空 = 明文存储）"))

	var (
		issueDir, issueName string
		issueClient         bool
		issueDays           int
		issueSANs           []string
	)
	issueCmd := &cobra.Command{
		Use: "issue --name <名称>",
		Short: i18n.T("issue a certificate (server by default; --client for the controller side; --san adds extra addresses)",
			"签发证书（默认服务端；--client 签控制端客户端证书；--san 追加多地址）"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if issueName == "" {
				return fmt.Errorf("%s", i18n.T("--name is required", "需要 --name"))
			}
			crt, key, fp, err := ca.Issue(issueDir, issueName, issueSANs, issueClient, issueDays)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, crt)
			fmt.Fprintln(out, key)
			fmt.Fprintf(out, "%s: %s（agent --pin-client-fp %s）\n",
				i18n.T("fingerprint", "指纹"), fp, i18n.T("enables exact revocation", "可精确吊销"))
			return nil
		},
	}
	issueCmd.Flags().StringVarP(&issueDir, "dir", "d", ".", i18n.T("CA directory", "CA 目录"))
	issueCmd.Flags().StringVar(&issueName, "name", "",
		i18n.T("certificate name (also used as SAN for server certs; IPs allowed)",
			"证书名称（服务端证书同时作为 SAN；IP 亦可）"))
	issueCmd.Flags().StringArrayVar(&issueSANs, "san", nil,
		i18n.T("additional SANs, repeatable (IP or hostname) — one cert covers all addresses of multi-homed/NAT/port-forwarded hosts",
			"附加 SAN，可多次（IP 或主机名）——多地址/NAT/端口转发主机一张证书覆盖全部可达地址"))
	issueCmd.Flags().BoolVar(&issueClient, "client", false,
		i18n.T("issue a controller-side client certificate", "签发控制端客户端证书"))
	issueCmd.Flags().IntVar(&issueDays, "days", ca.DefaultDays, i18n.T("validity in days", "有效期天数"))

	var (
		renewDir, renewName string
		renewNewKey         bool
		renewDays           int
	)
	renewCmd := &cobra.Command{
		Use: "renew --name <名称>",
		Short: i18n.T("renew a certificate (keeps SAN/EKU/private key; --new-key rotates it)",
			"续期证书（保留 SAN/EKU/私钥；--new-key 换新私钥）"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if renewName == "" {
				return fmt.Errorf("%s", i18n.T("--name is required", "需要 --name"))
			}
			crt, key, fp, err := ca.Renew(renewDir, renewName, renewNewKey, renewDays)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, crt)
			fmt.Fprintln(out, key)
			fmt.Fprintf(out, "%s: %s（%s）\n", i18n.T("fingerprint", "指纹"), fp,
				i18n.T("update pinned fingerprints on agents if --pin-client-fp is configured", "若 agent 配置了 --pin-client-fp 需同步更新"))
			return nil
		},
	}
	renewCmd.Flags().StringVarP(&renewDir, "dir", "d", ".", i18n.T("CA directory", "CA 目录"))
	renewCmd.Flags().StringVar(&renewName, "name", "",
		i18n.T("original certificate name (<name>.crt / <name>.key)", "原证书名称（<name>.crt / <name>.key）"))
	renewCmd.Flags().BoolVar(&renewNewKey, "new-key", false,
		i18n.T("rotate the private key on renewal (old cert becomes invalid immediately)",
			"续期时更换私钥（旧证书立即失效）"))
	renewCmd.Flags().IntVar(&renewDays, "days", ca.DefaultDays, i18n.T("new validity in days", "新有效期天数"))

	cmd.AddCommand(initCmd, issueCmd, renewCmd)
	return cmd
}
