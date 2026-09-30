package cli

import (
	"fmt"
	"strings"
	"time"

	"wdp/internal/ca"
	"wdp/internal/i18n"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// caHelp 返回 `wdp ca` 的长帮助（调用时求值）。
func caHelp() string {
	return i18n.T(`Self-managed CA and certificate issuing

init creates a root CA; issue signs certificates (server/client/peer profiles); renew extends
validity; show inspects certificate details
Used for agent mTLS, push session CAs and similar scenarios
`, `自管 CA 与证书签发工具

init 创建根 CA；issue 签发证书（server/client/peer 用途）；renew 延期；show 查看证书详情
用于 agent mTLS、push 会话 CA 等场景
`)
}

// newCACmd 构造 `wdp ca`（mTLS 证书工具）。
func newCACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: i18n.T("self-managed CA and certificate issuing", "自管 CA 与证书签发"),
		Long:  caHelp(),
	}
	cmd.AddCommand(newCAInitCmd(), newCAIssueCmd(), newCARenewCmd(), newCAShowCmd())
	return cmd
}

// caInitHelp 返回 `wdp ca init` 的长帮助（调用时求值）。
func caInitHelp() string {
	return i18n.T(`Create a fresh root CA

Generates the CA certificate and private key (<name>.crt/.key) and prints their paths and the certificate fingerprint
--name sets the file name (default ca); --days the validity in days; --path-len the intermediate CA depth
(0 = default, leaves only and no intermediate CA; N>0 allows N levels; -1 = unlimited)
Subject (--cn/--o/--ou/--c/--st/--l) and key spec (--key-algo ed25519|ecdsa|rsa, --key-size) are customizable

Examples:
wdp ca init ./ca-dir --days 3650
`, `创建全新根 CA

生成 CA 证书与私钥（<name>.crt/.key），输出路径与证书指纹
--name 文件名（默认 ca）；--days 有效期；--path-len 中间 CA 深度
（0 = 默认，只签叶子不允许中间 CA；N>0 允许 N 层；-1 不限）
主题（--cn/--o/--ou/--c/--st/--l）与密钥规格（--key-algo ed25519|ecdsa|rsa、--key-size）可定制

示例：
wdp ca init ./ca-dir --days 3650
`)
}

// newCAInitCmd 生成全新根 CA
func newCAInitCmd() *cobra.Command {
	CAInitOptions := ca.InitOptions{Dir: "."}
	cmd := &cobra.Command{
		Use:  "init [new-ca-path]",
		Args: cobra.MaximumNArgs(1),
		Short: i18n.T("create new root CA to dir",
			"在指定目录创建全新根 CA"),
		Long: caInitHelp(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				CAInitOptions.Dir = args[0]
			}
			crt, key, fp, err := ca.Init(CAInitOptions)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, crt)
			fmt.Fprintln(out, key)
			fmt.Fprintf(out, "%s: %s\n", "CA fingerprint", fp)
			return nil
		},
	}
	cmd.Flags().StringVarP(&CAInitOptions.Name, "name", "n", "", i18n.T(
		"CA file name (output <name>.crt/.key; default ca)",
		"CA 文件名（输出 <name>.crt/.key；默认 ca）"))
	cmd.Flags().IntVar(&CAInitOptions.Days, "days", ca.DefaultCADays, "CA validity in days")
	cmd.Flags().IntVar(&CAInitOptions.PathLen, "path-len", 0, i18n.T(
		"max intermediate CA depth: 0 = default (leaves only, no intermediate CA); N>0 allows N levels; -1 = unlimited",
		"中间 CA 最大深度：0 = 默认（只签叶子，不允许中间 CA）；N>0 允许 N 层；-1 = 不限"))
	caSubjectFlags(cmd.Flags(), &CAInitOptions.Subject)
	caKeyFlags(cmd.Flags(), &CAInitOptions.Key)
	return cmd
}

// caIssueHelp 返回 `wdp ca issue` 的长帮助（调用时求值）。
func caIssueHelp() string {
	return i18n.T(`Sign a certificate with the CA

--profile picks the usage: server (ServerAuth) / client (ClientAuth) / peer (both)
--san is repeatable: a plain value is auto-detected as IP or DNS, the uri:/email: prefixes set URI/Email SANs —
one certificate covers every address of a multi-homed/NAT/port-forwarded host
--days sets the validity; 0 = follow the CA's remaining lifetime automatically (a short-lived CA is
followed, a long-lived one is capped at 30d, and the CA expiry is never exceeded)
--ca-cert/--ca-key name the signing CA (default <dir>/ca.crt and <dir>/ca.key)
--name sets the certificate file name and the default CN (--cn overrides the CN without touching the file name)
Prints the certificate, key path and fingerprint; agent --pin-client-fp <fingerprint> enables exact client revocation

Examples:
wdp ca issue ./ca-dir --profile server --san 10.0.0.11 --san web1.example.com
`, `用 CA 签发证书

--profile 选择用途：server（ServerAuth）/ client（ClientAuth）/ peer（两者）
--san 可重复：裸值自动识别 IP 或 DNS，uri:/email: 前缀指定 URI/Email SAN——
多宿主/NAT/端口转发的主机一张证书覆盖全部地址
--days 有效期；0 = 自动跟随 CA 剩余有效期（短 CA 跟随、长 CA 封顶 30d，绝不超出 CA 到期）
--ca-cert/--ca-key 指定签发 CA（默认 <dir>/ca.crt 与 <dir>/ca.key）
--name 证书文件名与默认 CN（--cn 可覆盖 CN，不影响文件名）
输出证书、私钥路径与指纹；agent --pin-client-fp <指纹> 可开启客户端精确吊销

示例：
wdp ca issue ./ca-dir --profile server --san 10.0.0.11 --san web1.example.com
`)
}

// newCAIssueCmd 构造 `wdp ca issue`。
func newCAIssueCmd() *cobra.Command {
	CAIssueOptions := ca.IssueOptions{Dir: "."}
	var profile, name string

	cmd := &cobra.Command{
		Use:  "issue [new-ca-path]",
		Args: cobra.MaximumNArgs(1),
		Short: i18n.T("issue a certificate",
			"用 CA 签发证书"),
		Long: caIssueHelp(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				CAIssueOptions.Dir = args[0]
			}
			prof, err := ca.NormalizeProfile(profile)
			if err != nil {
				return err
			}
			CAIssueOptions.Profile = prof
			crt, keyPath, fp, err := ca.Issue(CAIssueOptions, name)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, crt)
			fmt.Fprintln(out, keyPath)
			fmt.Fprintf(out, "%s: %s(agent --pin-client-fp %s)\n",
				"fingerprint", fp, "enables exact revocation")
			return nil
		},
	}
	cmd.Flags().StringVar(&CAIssueOptions.CACertPath, "ca-cert", "", i18n.T(
		"CA certificate that SIGNS; defaults to <dir>/ca.crt",
		"用于签发的 CA 证书；默认 <dir>/ca.crt"))
	cmd.Flags().StringVar(&CAIssueOptions.CAKeyPath, "ca-key", "", i18n.T(
		"CA private key that SIGNS (default <dir>/ca.key)",
		"用于签发的 CA 私钥（默认 <dir>/ca.key）"))
	cmd.Flags().StringVar(&name, "name", "", i18n.T(
		"certificate name: output file <name>.crt/.key and the default CommonName (--cn overrides CN); empty = the profile name (server/client/peer)",
		"证书名：输出文件 <name>.crt/.key 与默认 CommonName（--cn 可覆盖 CN）；留空 = 用 profile 名（server/client/peer）"))
	cmd.Flags().StringVar(&profile, "profile", "server", i18n.T(
		"usage profile: server (ServerAuth) | client (ClientAuth) | peer (both)",
		"用途档位：server（ServerAuth）| client（ClientAuth）| peer（两者）"))
	cmd.Flags().StringSliceVar(&CAIssueOptions.SANs, "san", nil, i18n.T(
		"additional SANs, repeatable — plain value: IP or DNS (auto-detected); uri:… and email:… prefixes for URI/Email SAN — one cert covers all addresses of multi-homed/NAT/port-forwarded hosts",
		"附加 SAN，可重复——裸值自动识别为 IP 或 DNS；uri:… 与 email:… 前缀指定 URI/Email SAN——多宿主/NAT/端口转发的主机一张证书覆盖全部地址"))
	cmd.Flags().IntVar(&CAIssueOptions.Days, "days", 0, i18n.T(
		"validity in days; 0 = auto (follow the CA expiry when it is short: <=30d; 50% of remaining for 30-60d; capped at 30d for long-lived CAs; never exceeds the CA expiry)",
		"有效期（天）；0 = 自动（短 CA 跟随其剩余有效期：<=30d；30-60d 取剩余的一半；长 CA 封顶 30d；绝不超出 CA 到期）"))
	caSubjectFlags(cmd.Flags(), &CAIssueOptions.Subject)
	caKeyFlags(cmd.Flags(), &CAIssueOptions.Key)
	return cmd
}

// caSubjectFlags 注册主题 flags（init/issue 共用）
func caSubjectFlags(f *pflag.FlagSet, subj *ca.Subject) {
	f.StringVar(&subj.CN, "cn", "", i18n.T(
		"CommonName (init: default wdp-ca; issue: overrides the CN derived from --name, file name unaffected)",
		"CommonName（init：默认 wdp-ca；issue：覆盖由 --name 推导的 CN，不影响文件名）"))
	f.StringArrayVar(&subj.O, "o", nil, "organization, repeatable (default wdp)")
	f.StringArrayVar(&subj.OU, "ou", nil, "organizational unit, repeatable")
	f.StringArrayVar(&subj.C, "c", nil, "country code (2 letters), repeatable")
	f.StringArrayVar(&subj.ST, "st", nil, "state/province, repeatable")
	f.StringArrayVar(&subj.L, "l", nil, "locality (city), repeatable")
}

// caKeyFlags 注册密钥规格 flags（init/issue 共用）。
func caKeyFlags(f *pflag.FlagSet, key *ca.KeySpec) {
	f.StringVar(&key.Algo, "key-algo", "ed25519", i18n.T(
		"key algorithm: ed25519 (default) | ecdsa | rsa",
		"密钥算法：ed25519（默认）| ecdsa | rsa"))
	f.IntVar(&key.Size, "key-size", 0, i18n.T(
		"key size: rsa 2048/3072/4096, ecdsa 256/384/521, ed25519 n/a (0 = default per algo)",
		"密钥长度：rsa 2048/3072/4096，ecdsa 256/384/521，ed25519 不适用（0 = 按算法取默认）"))
}

// caRenewHelp 返回 `wdp ca renew` 的长帮助（调用时求值）。
func caRenewHelp() string {
	return i18n.T(`Extend a certificate's validity

--cert names the certificate to renew (required); --key is its matching private key (optional with --new-key)
--days is ADDED to the current expiry (default 30) and is still capped by the CA expiry
--new-key rotates the private key (same algorithm); --ca-cert/--ca-key name the re-signing CA
(default: ca.crt/ca.key in the output directory)
Output goes to ./<old certificate file name> by default; a positional argument sets the output path

Examples:
wdp ca renew --cert server.crt --key server.key --days 60
`, `延长证书有效期

--cert 指定要延期的证书（必填）；--key 为其配对私钥（--new-key 时可省）
--days 是在当前到期时间上增加的天数（默认 30），仍被 CA 到期时间封顶
--new-key 轮换私钥（算法不变）；--ca-cert/--ca-key 指定重签名 CA（默认输出目录下的 ca.crt/ca.key）
产物默认写到 ./<旧证书文件名>，位置参数可指定输出路径

示例：
wdp ca renew --cert server.crt --key server.key --days 60
`)
}

// newCARenewCmd 构造 `wdp ca renew`。
func newCARenewCmd() *cobra.Command {
	// OutPath 留空 = ca.Renew 语义的 "./<旧证书文件名>"；置 "." 会被推导成
	// 证书名 "."，产物落到 ..crt/.key 隐藏文件
	CARenewOptions := ca.RenewOptions{}
	cmd := &cobra.Command{
		Use: "renew [new-ca-path]",
		Short: i18n.T("extend a certificate's expiry",
			"延长证书有效期"),
		Long: caRenewHelp(),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				CARenewOptions.OutPath = args[0]
			}
			crt, key, fp, err := ca.Renew(CARenewOptions)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, crt)
			fmt.Fprintln(out, key)
			fmt.Fprintf(out, "%s: %s\n", "fingerprint", fp)
			return nil
		},
	}
	cmd.Flags().StringVar(&CARenewOptions.CertPath, "cert", "", "the certificate to renew (required)")
	cmd.Flags().StringVar(&CARenewOptions.KeyPath, "key", "", i18n.T(
		"the certificate's private key (required unless --new-key; verified to pair with --cert)",
		"证书的配对私钥（除 --new-key 外必填；会校验与 --cert 配对）"))
	cmd.Flags().StringVar(&CARenewOptions.CACertPath, "ca-cert", "", i18n.T(
		"ROOT CA certificate that re-signs (default: ca.crt in the output directory)",
		"重签名的根 CA 证书（默认：输出目录下的 ca.crt）"))
	cmd.Flags().StringVar(&CARenewOptions.CAKeyPath, "ca-key", "", i18n.T(
		"ROOT CA private key (default: ca.key in the output directory)",
		"根 CA 私钥（默认：输出目录下的 ca.key）"))
	cmd.Flags().BoolVar(&CARenewOptions.NewKey, "new-key", false, i18n.T(
		"rotate the private key instead of reusing (same algorithm; --key not needed)",
		"轮换私钥而不是复用（算法不变；此时无需 --key）"))
	cmd.Flags().IntVar(&CARenewOptions.Days, "days", ca.DefaultDays, i18n.T(
		"days to ADD to the current expiry (default 30; still clamped to the CA expiry)",
		"在当前到期时间上增加的天数（默认 30；仍被 CA 到期时间封顶）"))
	_ = cmd.MarkFlagRequired("cert")
	return cmd
}

// caShowHelp 返回 `wdp ca show` 的长帮助（调用时求值）。
func caShowHelp() string {
	return i18n.T(`Show the information a certificate carries

Prints subject/issuer (self-signed is marked), serial number, validity (days left / expired flag),
CA role and PathLen depth, signature and public-key algorithms, key usage, SANs and the SHA256 fingerprint
--key additionally verifies that the private key pairs with the certificate's public key (non-zero exit on mismatch)

Examples:
wdp ca show ca.crt
wdp ca show server.crt --key server.key
`, `查看证书携带的信息

输出主题/签发者（自签名标注）、序列号、有效期（剩余天数/过期标记）、
CA 角色与 PathLen 深度、签名与公钥算法、密钥用途、SAN、SHA256 指纹
--key 同时验证私钥与证书公钥配对（不匹配则非零退出）

示例：
wdp ca show ca.crt
wdp ca show server.crt --key server.key
`)
}

// newCAShowCmd 构造 `wdp ca show`（查看证书携带的信息）。
func newCAShowCmd() *cobra.Command {
	var keyPath string
	cmd := &cobra.Command{
		Use: "show <CAFilePath>",
		Short: i18n.T("show certificate details",
			"查看证书详情"),
		Long: caShowHelp(),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := ca.Inspect(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			issuer := info.Issuer
			if info.SelfSigned {
				issuer += " (self-signed)"
			}
			validity := fmt.Sprintf("%s ~ %s",
				info.NotBefore.Local().Format("2006-01-02 15:04"),
				info.NotAfter.Local().Format("2006-01-02 15:04"))
			if left := time.Until(info.NotAfter); left < 0 {
				validity += " (EXPIRED)"
			} else {
				validity += fmt.Sprintf(" (%d days left)", int(left.Hours()/24)+1)
			}

			rows := []struct{ label, value string }{
				{"certificate", info.Path},
				{"subject", info.Subject},
				{"issuer", issuer},
				{"serial", info.Serial},
				{"validity", validity},
				{"CA", caRole(info)},
				{"signature", info.Signature},
				{"public key", info.PublicKey},
			}
			if len(info.KeyUsage) > 0 {
				rows = append(rows, struct{ label, value string }{
					"key usage", strings.Join(info.KeyUsage, ", ")})
			}
			if len(info.ExtKeyUsage) > 0 {
				rows = append(rows, struct{ label, value string }{
					"extended usage", strings.Join(info.ExtKeyUsage, ", ")})
			}
			if len(info.DNSNames) > 0 || len(info.IPs) > 0 || len(info.URIs) > 0 || len(info.Emails) > 0 {
				var sans []string
				for _, d := range info.DNSNames {
					sans = append(sans, "DNS:"+d)
				}
				for _, u := range info.URIs {
					sans = append(sans, "URI:"+u)
				}
				for _, e := range info.Emails {
					sans = append(sans, "email:"+e)
				}
				for _, ip := range info.IPs {
					sans = append(sans, "IP:"+ip)
				}
				rows = append(rows, struct{ label, value string }{
					"SAN", strings.Join(sans, ", ")})
			}
			rows = append(rows, struct{ label, value string }{
				"fingerprint", info.Fingerprint})

			for _, r := range rows {
				fmt.Fprintf(out, "%s: %s\n", r.label, r.value)
			}
			if keyPath != "" {
				if err := ca.VerifyKeyPair(args[0], keyPath); err != nil {
					return err
				}
				fmt.Fprintf(out, "key pair: verified (%s matches the certificate)\n", keyPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&keyPath, "key", "", i18n.T(
		"private key path: verify it pairs with the certificate's public key (non-zero exit on mismatch)",
		"私钥路径：验证其与证书公钥配对（不匹配则非零退出）"))
	return cmd
}

// caRole 描述证书角色（CA 深度 / 叶子）。
func caRole(info *ca.CertInfo) string {
	if !info.IsCA {
		return "no (leaf certificate)"
	}
	if info.PathLenZero {
		return "yes (PathLen=0, no intermediate CA)"
	}
	if info.PathLen < 0 {
		return "yes (no path limit)"
	}
	return fmt.Sprintf("yes (PathLen=%d)", info.PathLen)
}
