package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

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
	cmd.AddCommand(newCAInitCmd(), newCAIssueCmd(), newCARenewCmd(), newCAShowCmd())
	return cmd
}

// newCAInitCmd 构造 `wdp ca init`。
func newCAInitCmd() *cobra.Command {
	var dir, passphrase, importCert, importKey string
	cmd := &cobra.Command{
		Use: "init",
		Short: i18n.T("initialize a CA (ca.crt / ca.key); --import-cert/--import-key reuse an existing CA",
			"初始化 CA（ca.crt / ca.key）；--import-cert/--import-key 导入已有 CA"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var (
				crt, key, fp string
				err          error
			)
			switch {
			case importCert != "" || importKey != "":
				if importCert == "" || importKey == "" {
					return errors.New(i18n.T("--import-cert and --import-key must be given together",
						"--import-cert 与 --import-key 需同时指定"))
				}
				crt, key, fp, err = ca.Import(dir, importCert, importKey, passphrase)
			default:
				crt, key, fp, err = ca.Init(dir, passphrase)
			}
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
	cmd.Flags().StringVarP(&dir, "dir", "d", ".", i18n.T("CA directory", "CA 目录"))
	cmd.Flags().StringVar(&passphrase, "passphrase", "",
		i18n.T("CA private key passphrase (default from "+ca.PassphraseEnv+" env; empty = stored in plaintext)",
			"CA 私钥口令（缺省取 "+ca.PassphraseEnv+" 环境变量；空 = 明文存储）"))
	cmd.Flags().StringVar(&importCert, "import-cert", "",
		i18n.T("import an existing CA certificate instead of generating a new one (with --import-key)",
			"导入已有 CA 证书而非新生成（需与 --import-key 同时指定）"))
	cmd.Flags().StringVar(&importKey, "import-key", "",
		i18n.T("import an existing CA private key (plaintext SEC1/PKCS8, or wdp-encrypted envelope decrypted by --passphrase)",
			"导入已有 CA 私钥（明文 SEC1/PKCS8；wdp 加密信封用 --passphrase 解密）"))
	return cmd
}

// newCAIssueCmd 构造 `wdp ca issue`。
func newCAIssueCmd() *cobra.Command {
	var (
		dir        string
		caCert     string
		caKey      string
		passphrase string
		name       string
		client     bool
		days       int
		sans       []string
	)
	cmd := &cobra.Command{
		Use: "issue --name <name>",
		Short: i18n.T("issue a certificate (server by default; --client for the controller side; --san adds extra addresses)",
			"签发证书（默认服务端；--client 签控制端客户端证书；--san 追加多地址）"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("%s", i18n.T("--name is required", "需要 --name"))
			}
			crt, key, fp, err := ca.Issue(ca.IssueOptions{
				Dir:        dir,
				CACertPath: caCert,
				CAKeyPath:  caKey,
				Passphrase: passphrase,
				SANs:       sans,
				Client:     client,
				Days:       days,
			}, name)
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
	cmd.Flags().StringVarP(&dir, "dir", "d", ".", i18n.T("CA directory", "CA 目录"))
	cmd.Flags().StringVar(&caCert, "ca-cert", "",
		i18n.T("CA certificate path (default <dir>/ca.crt)",
			"CA 证书路径（默认 <dir>/ca.crt）"))
	cmd.Flags().StringVar(&caKey, "ca-key", "",
		i18n.T("CA private key path (default <dir>/ca.key)",
			"CA 私钥路径（默认 <dir>/ca.key）"))
	cmd.Flags().StringVar(&passphrase, "passphrase", "",
		i18n.T("CA private key passphrase (default from "+ca.PassphraseEnv+" env)",
			"CA 私钥口令（缺省取 "+ca.PassphraseEnv+" 环境变量）"))
	cmd.Flags().StringVar(&name, "name", "",
		i18n.T("certificate name (also used as SAN for server certs; IPs allowed)",
			"证书名称（服务端证书同时作为 SAN；IP 亦可）"))
	cmd.Flags().StringSliceVar(&sans, "san", nil,
		i18n.T("additional SANs, repeatable (IP or hostname) — one cert covers all addresses of multi-homed/NAT/port-forwarded hosts",
			"附加 SAN，可多次（IP 或主机名）——多地址/NAT/端口转发主机一张证书覆盖全部可达地址"))
	cmd.Flags().BoolVar(&client, "client", false,
		i18n.T("issue a controller-side client certificate",
			"签发控制端客户端证书"))
	cmd.Flags().IntVar(&days, "days", ca.DefaultDays, i18n.T("validity in days", "有效期天数"))
	return cmd
}

// newCARenewCmd 构造 `wdp ca renew`。
func newCARenewCmd() *cobra.Command {
	var (
		dir        string
		caCert     string
		caKey      string
		passphrase string
		name       string
		newKey     bool
		days       int
	)
	cmd := &cobra.Command{
		Use: "renew --name <名称>",
		Short: i18n.T("renew a certificate (keeps SAN/EKU/private key; --new-key rotates it)",
			"续期证书（保留 SAN/EKU/私钥；--new-key 换新私钥）"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("%s", i18n.T("--name is required", "需要 --name"))
			}
			crt, key, fp, err := ca.Renew(ca.RenewOptions{
				Dir:        dir,
				CACertPath: caCert,
				CAKeyPath:  caKey,
				Passphrase: passphrase,
				NewKey:     newKey,
				Days:       days,
			}, name)
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
	cmd.Flags().StringVarP(&dir, "dir", "d", ".", i18n.T("CA directory", "CA 目录"))
	cmd.Flags().StringVar(&caCert, "ca-cert", "",
		i18n.T("CA certificate path (default <dir>/ca.crt)",
			"CA 证书路径（默认 <dir>/ca.crt）"))
	cmd.Flags().StringVar(&caKey, "ca-key", "",
		i18n.T("CA private key path (default <dir>/ca.key)",
			"CA 私钥路径（默认 <dir>/ca.key）"))
	cmd.Flags().StringVar(&passphrase, "passphrase", "",
		i18n.T("CA private key passphrase (default from "+ca.PassphraseEnv+" env)",
			"CA 私钥口令（缺省取 "+ca.PassphraseEnv+" 环境变量）"))
	cmd.Flags().StringVar(&name, "name", "",
		i18n.T("original certificate name (<name>.crt / <name>.key)", "原证书名称（<name>.crt / <name>.key）"))
	cmd.Flags().BoolVar(&newKey, "new-key", false,
		i18n.T("rotate the private key on renewal (old cert becomes invalid immediately)",
			"续期时更换私钥（旧证书立即失效）"))
	cmd.Flags().IntVar(&days, "days", ca.DefaultDays, i18n.T("new validity in days", "新有效期天数"))
	return cmd
}

// newCAShowCmd 构造 `wdp ca show`（查看证书携带的信息）。
func newCAShowCmd() *cobra.Command {
	return &cobra.Command{
		Use: "show <证书路径>",
		Short: i18n.T("show certificate details (subject, validity, SAN, key usage, fingerprint)",
			"查看证书携带的信息（主题、有效期、SAN、密钥用途、指纹）"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := ca.Inspect(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			issuer := info.Issuer
			if info.SelfSigned {
				issuer += i18n.T(" (self-signed)", "（自签）")
			}
			validity := fmt.Sprintf("%s ~ %s",
				info.NotBefore.Local().Format("2006-01-02 15:04"),
				info.NotAfter.Local().Format("2006-01-02 15:04"))
			if left := time.Until(info.NotAfter); left < 0 {
				validity += i18n.T(" (EXPIRED)", "（已过期）")
			} else {
				validity += fmt.Sprintf(i18n.T(" (%d days left)", "（剩余 %d 天）"), int(left.Hours()/24)+1)
			}

			rows := []struct{ label, value string }{
				{i18n.T("certificate", "证书"), info.Path},
				{i18n.T("subject", "主题"), info.Subject},
				{i18n.T("issuer", "签发者"), issuer},
				{i18n.T("serial", "序列号"), info.Serial},
				{i18n.T("validity", "有效期"), validity},
				{i18n.T("CA", "CA"), caRole(info)},
				{i18n.T("public key", "公钥"), info.PublicKey},
			}
			if len(info.KeyUsage) > 0 {
				rows = append(rows, struct{ label, value string }{
					i18n.T("key usage", "密钥用途"), strings.Join(info.KeyUsage, ", ")})
			}
			if len(info.ExtKeyUsage) > 0 {
				rows = append(rows, struct{ label, value string }{
					i18n.T("extended usage", "扩展用途"), strings.Join(info.ExtKeyUsage, ", ")})
			}
			if len(info.DNSNames) > 0 || len(info.IPs) > 0 {
				sans := make([]string, 0, len(info.DNSNames)+len(info.IPs))
				for _, d := range info.DNSNames {
					sans = append(sans, "DNS:"+d)
				}
				for _, ip := range info.IPs {
					sans = append(sans, "IP:"+ip)
				}
				rows = append(rows, struct{ label, value string }{
					i18n.T("SAN", "SAN"), strings.Join(sans, ", ")})
			}
			rows = append(rows, struct{ label, value string }{
				i18n.T("fingerprint", "指纹"), info.Fingerprint})

			for _, r := range rows {
				fmt.Fprintf(out, "%s: %s\n", r.label, r.value)
			}
			return nil
		},
	}
}

// caRole 描述证书角色（CA 深度 / 叶子）。
func caRole(info *ca.CertInfo) string {
	if !info.IsCA {
		return i18n.T("no (leaf certificate)", "否（叶子证书）")
	}
	if info.PathLenZero {
		return i18n.T("yes (PathLen=0, no intermediate CA)", "是（PathLen=0，禁止签发中间 CA）")
	}
	if info.PathLen < 0 {
		return i18n.T("yes (no path limit)", "是（深度不限）")
	}
	return fmt.Sprintf(i18n.T("yes (PathLen=%d)", "是（PathLen=%d）"), info.PathLen)
}
