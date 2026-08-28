package cli

import (
	"fmt"
	"strings"
	"time"

	"wdp/internal/ca"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newCACmd 构造 `wdp ca`（mTLS 证书工具）。
func newCACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "self-managed CA and certificate issuing",
	}
	cmd.AddCommand(newCAInitCmd(), newCAIssueCmd(), newCARenewCmd(), newCAShowCmd())
	return cmd
}

// newCAInitCmd 生成全新根 CA
func newCAInitCmd() *cobra.Command {
	CAInitOptions := ca.InitOptions{Dir: "."}
	cmd := &cobra.Command{
		Use:   "init [new-ca-path]",
		Args:  cobra.MaximumNArgs(1),
		Short: "create new root CA to dir",
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
	cmd.Flags().StringVarP(&CAInitOptions.Name, "name", "n", "", "CA file name (output <name>.crt/.key; default ca)")
	cmd.Flags().IntVar(&CAInitOptions.Days, "days", ca.DefaultCADays, "CA validity in days")
	cmd.Flags().IntVar(&CAInitOptions.PathLen, "path-len", 0, "max intermediate CA depth: 0 = default (leaves only, no intermediate CA); N>0 allows N levels; -1 = unlimited")
	caSubjectFlags(cmd.Flags(), &CAInitOptions.Subject)
	caKeyFlags(cmd.Flags(), &CAInitOptions.Key)
	return cmd
}

// newCAIssueCmd 构造 `wdp ca issue`。
func newCAIssueCmd() *cobra.Command {
	CAIssueOptions := ca.IssueOptions{Dir: "."}
	var profile, name string

	cmd := &cobra.Command{
		Use:   "issue [new-ca-path]",
		Args:  cobra.MaximumNArgs(1),
		Short: "issue a certificate",
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
	cmd.Flags().StringVar(&CAIssueOptions.CACertPath, "ca-cert", "", "CA certificate that SIGNS; defaults to <dir>/ca.crt")
	cmd.Flags().StringVar(&CAIssueOptions.CAKeyPath, "ca-key", "", "CA private key that SIGNS (default <dir>/ca.key)")
	cmd.Flags().StringVar(&name, "name", "", "certificate name: output file <name>.crt/.key and the default CommonName (--cn overrides CN); empty = the profile name (server/client/peer)")
	cmd.Flags().StringVar(&profile, "profile", "server", "usage profile: server (ServerAuth) | client (ClientAuth) | peer (both)")
	cmd.Flags().StringSliceVar(&CAIssueOptions.SANs, "san", nil, "additional SANs, repeatable — plain value: IP or DNS (auto-detected); uri:… and email:… prefixes for URI/Email SAN — one cert covers all addresses of multi-homed/NAT/port-forwarded hosts")
	cmd.Flags().IntVar(&CAIssueOptions.Days, "days", 0, "validity in days; 0 = auto (follow the CA expiry when it is short: <=30d; 50%% of remaining for 30-60d; capped at 30d for long-lived CAs; never exceeds the CA expiry)")
	caSubjectFlags(cmd.Flags(), &CAIssueOptions.Subject)
	caKeyFlags(cmd.Flags(), &CAIssueOptions.Key)
	return cmd
}

// caSubjectFlags 注册主题 flags（init/issue 共用）
func caSubjectFlags(f *pflag.FlagSet, subj *ca.Subject) {
	f.StringVar(&subj.CN, "cn", "", "CommonName (init: default wdp-ca; issue: overrides the CN derived from --name, file name unaffected)")
	f.StringArrayVar(&subj.O, "o", nil, "organization, repeatable (default wdp)")
	f.StringArrayVar(&subj.OU, "ou", nil, "organizational unit, repeatable")
	f.StringArrayVar(&subj.C, "c", nil, "country code (2 letters), repeatable")
	f.StringArrayVar(&subj.ST, "st", nil, "state/province, repeatable")
	f.StringArrayVar(&subj.L, "l", nil, "locality (city), repeatable")
}

// caKeyFlags 注册密钥规格 flags（init/issue 共用）。
func caKeyFlags(f *pflag.FlagSet, key *ca.KeySpec) {
	f.StringVar(&key.Algo, "key-algo", "ed25519", "key algorithm: ed25519 (default) | ecdsa | rsa")
	f.IntVar(&key.Size, "key-size", 0, "key size: rsa 2048/3072/4096, ecdsa 256/384/521, ed25519 n/a (0 = default per algo)")
}

// newCARenewCmd 构造 `wdp ca renew`。
func newCARenewCmd() *cobra.Command {
	CARenewOptions := ca.RenewOptions{OutPath: "."}
	cmd := &cobra.Command{
		Use:   "renew [new-ca-path]",
		Short: "extend a certificate's expiry",
		Args:  cobra.MaximumNArgs(1),
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
	cmd.Flags().StringVar(&CARenewOptions.KeyPath, "key", "", "the certificate's private key (required unless --new-key; verified to pair with --cert)")
	cmd.Flags().StringVar(&CARenewOptions.CACertPath, "ca-cert", "", "ROOT CA certificate that re-signs (default: ca.crt in the output directory)")
	cmd.Flags().StringVar(&CARenewOptions.CAKeyPath, "ca-key", "", "ROOT CA private key (default: ca.key in the output directory)")
	cmd.Flags().BoolVar(&CARenewOptions.NewKey, "new-key", false, "rotate the private key instead of reusing (same algorithm; --key not needed)")
	cmd.Flags().IntVar(&CARenewOptions.Days, "days", ca.DefaultDays, "days to ADD to the current expiry (default 30; still clamped to the CA expiry)")
	_ = cmd.MarkFlagRequired("cert")
	return cmd
}

// newCAShowCmd 构造 `wdp ca show`（查看证书携带的信息）。
func newCAShowCmd() *cobra.Command {
	var keyPath string
	cmd := &cobra.Command{
		Use:   "show <CAFilePath>",
		Short: "show certificate details",
		Args:  cobra.ExactArgs(1),
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
	cmd.Flags().StringVar(&keyPath, "key", "", "private key path: verify it pairs with the certificate's public key (non-zero exit on mismatch)")
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
