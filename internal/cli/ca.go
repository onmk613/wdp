package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/ca"
	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
	"wdp/internal/release"
)

func printYAML(out io.Writer, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	return err
}

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

// newReleaseCmd 构造 `wdp release`（部署记录）。
func newReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: i18n.T("deployment record viewing (list / show)", "部署记录查看（list / show）"),
	}
	listCmd := &cobra.Command{
		Use:   "list [chart名前缀]",
		Short: "列出部署记录（新在前）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filter := ""
			if len(args) > 0 {
				filter = args[0]
			}
			recs, err := release.List(filter)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(recs) == 0 {
				fmt.Fprintln(out, i18n.T("(no records)", "（无记录）"))
				return nil
			}
			tb := outPrinter(cmd).NewTable("ID", i18n.T("VERSION", "版本"), i18n.T("RESULT", "结果"), i18n.T("TIME", "时间"))
			for _, r := range recs {
				if r.Failed {
					tb.AddRow(fmtutil.C(r.ID), fmtutil.C(r.Version),
						fmtutil.CC(i18n.T("failed", "失败"), fmtutil.Red), fmtutil.C(r.Time.Format("2006-01-02 15:04:05")))
				} else {
					tb.AddRow(fmtutil.C(r.ID), fmtutil.C(r.Version),
						fmtutil.CC(i18n.T("ok", "成功"), fmtutil.Green), fmtutil.C(r.Time.Format("2006-01-02 15:04:05")))
				}
			}
			tb.Render()
			return nil
		},
	}

	var asValues bool
	showCmd := &cobra.Command{
		Use:   "show <id>",
		Short: "查看记录详情（--values 输出 values 快照 YAML，可直接 -f 重放）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := release.Load(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asValues {
				if rec.Values == nil {
					return fmt.Errorf("该记录无 values（裸 playbook 模式）")
				}
				return printYAML(out, rec.Values)
			}
			fmt.Fprintf(out, "ID:     %s\n时间:  %s\nchart:  %s %s\n结果:  %s\n主机:  %v\n",
				rec.ID, rec.Time.Format("2006-01-02 15:04:05"), rec.Chart, rec.Version,
				boolLabel(rec.Failed), rec.Hosts)
			if len(rec.ValuesRef) > 0 {
				fmt.Fprintf(out, "参数:  %v\n", rec.ValuesRef)
			}
			if rec.Values != nil {
				fmt.Fprintln(out, "\nvalues 快照（--values 查看完整内容）:")
				if err := printYAML(out, rec.Values); err != nil {
					return err
				}
			}
			return nil
		},
	}
	showCmd.Flags().BoolVar(&asValues, "values", false,
		i18n.T("print only the values snapshot (YAML)", "仅输出 values 快照（YAML）"))

	diffCmd := &cobra.Command{
		Use:   "diff <id1> <id2>",
		Short: "对比两次部署记录的 values 快照（升级前回答这次会改哪些参数）",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := release.Load(args[0])
			if err != nil {
				return err
			}
			b, err := release.Load(args[1])
			if err != nil {
				return err
			}
			if a.Values == nil || b.Values == nil {
				return fmt.Errorf("对比双方都需要 values 快照（裸 playbook 记录无法对比）")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "对比 %s（%s %s）→ %s（%s %s）\n",
				a.ID, a.Chart, a.Version, b.ID, b.Chart, b.Version)
			var lines []string
			diffValues("", a.Values, b.Values, &lines)
			if len(lines) == 0 {
				fmt.Fprintln(out, "values 完全一致（无参数变更）")
				return nil
			}
			for _, l := range lines {
				fmt.Fprintln(out, l)
			}
			return nil
		},
	}

	cmd.AddCommand(listCmd, showCmd, diffCmd)
	return cmd
}

// diffValues 递归对比两份 values，输出 - 路径: 旧值 / + 路径: 新值 行。
func diffValues(prefix string, a, b map[string]any, out *[]string) {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		av, aok := a[k]
		bv, bok := b[k]
		switch {
		case !aok:
			*out = append(*out, fmt.Sprintf("+ %s: %v（新增）", path, bv))
		case !bok:
			*out = append(*out, fmt.Sprintf("- %s: %v（删除）", path, av))
		default:
			am, aIsMap := av.(map[string]any)
			bm, bIsMap := bv.(map[string]any)
			if aIsMap && bIsMap {
				diffValues(path, am, bm, out)
				continue
			}
			if fmt.Sprint(av) != fmt.Sprint(bv) {
				*out = append(*out, fmt.Sprintf("- %s: %v", path, av))
				*out = append(*out, fmt.Sprintf("+ %s: %v", path, bv))
			}
		}
	}
}

func boolLabel(failed bool) string {
	if failed {
		return "失败"
	}
	return "成功"
}
