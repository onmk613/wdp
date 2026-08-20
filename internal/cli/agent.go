package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"wdp/internal/agent"
	"wdp/internal/config"
	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
	"wdp/internal/module"
	"wdp/internal/skel"
)

// newAgentCmd 构造 `wdp agent`：目标机常驻服务。
func newAgentCmd() *cobra.Command {
	var (
		listen        string
		token         string
		tokenFile     string
		ca, cert, key string
		cleanup       bool
		pins          []string
		allowNoAuth   bool
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: i18n.T("start the resident agent (on target hosts)", "启动常驻 agent（目标机）"),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 未显式指定时跟随 wdp.cfg [agent].port（默认仅绑定回环地址，对外监听需显式 --listen）
			if !cmd.Flags().Changed("listen") {
				listen = fmt.Sprintf("127.0.0.1:%d", config.Current().AgentPort())
			}
			srv := agent.New(listen)
			if err := srv.ConfigureAuth(token, tokenFile, ca, cert, key); err != nil {
				return err
			}
			if err := srv.PinClientFingerprints(pins); err != nil {
				return err
			}
			srv.CleanupOnShutdown(cleanup)
			srv.AllowNoAuth(allowNoAuth)
			mode := i18n.T("no auth", "无认证")
			if ca != "" && cert != "" && key != "" {
				mode = i18n.T("mutual TLS", "mTLS 双向认证")
				if len(pins) > 0 {
					mode += i18n.T(" + pinned clients", " +指纹准许名单")
				}
			} else if token != "" || tokenFile != "" {
				mode = i18n.T("token auth", "token 认证")
			} else if agent.IsLoopbackListen(listen) {
				mode = i18n.T("no auth (loopback only)", "无认证（仅本机回环）")
			}
			fmt.Printf(i18n.T("wdp agent %s listening on %s (%s)\n", "wdp agent %s 监听 %s（%s）\n"), Version, listen, mode)
			return srv.ListenAndServe()
		},
	}
	f := cmd.Flags()
	f.StringVar(&listen, "listen", "127.0.0.1:7602",
		i18n.T("listen address (default binds loopback only; use 0.0.0.0:PORT to expose)", "监听地址（默认仅绑定回环；对外监听需显式 0.0.0.0:端口）"))
	f.StringVar(&token, "token", "",
		i18n.T("auth token (lands in process argv, visible to ps; prefer --token-file)", "token 认证密钥（会进进程 argv，同机 ps 可见；建议改用 --token-file）"))
	f.StringVar(&tokenFile, "token-file", "",
		i18n.T("read token from file (file is deleted right after read)", "从文件读取 token（读取后自动删除文件）"))
	f.StringVar(&ca, "ca", "", i18n.T("mTLS: CA certificate (client certs must be issued by it)", "mTLS：CA 证书（客户端证书由该 CA 签发）"))
	f.StringVar(&cert, "cert", "", i18n.T("mTLS: server certificate", "mTLS：服务端证书"))
	f.StringVar(&key, "key", "", i18n.T("mTLS: server private key", "mTLS：服务端私钥"))
	f.BoolVar(&cleanup, "cleanup-on-shutdown", false,
		i18n.T("delete own binary on /shutdown then exit (for push temp agents)", "收到 /shutdown 时删除自身二进制后退出（push 临时 agent 用）"))
	f.StringArrayVar(&pins, "pin-client-fp", nil,
		i18n.T("allowed client cert SHA256 fingerprints (repeatable; exact revocation: drop a fingerprint and restart)",
			"客户端证书 SHA256 指纹准许名单（可多次；精确吊销：移除指纹重启即拒收）"))
	f.BoolVar(&allowNoAuth, "allow-no-auth", false,
		i18n.T("explicitly allow unauthenticated non-loopback listen (trusted LAN only)",
			"显式允许无认证对外监听（仅限可信内网）"))
	return cmd
}

// newModulesCmd 构造 `wdp modules`。
func newModulesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "modules [module]",
		Short: i18n.T("list built-in modules (with a module name, print parameter docs and example)",
			"列出内置模块（带模块名时输出参数文档与示例）"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				snippet, err := skel.ModuleSnippet(args[0])
				if err != nil {
					return err
				}
				fmt.Fprint(out, snippet)
				return nil
			}
			tb := outPrinter(cmd).NewTable(i18n.T("MODULE", "模块"), i18n.T("DESCRIPTION", "说明"))
			for _, name := range module.Names() {
				m, _ := module.Get(name)
				tb.AddRow(fmtutil.C(name), fmtutil.C(m.Desc()))
			}
			tb.Render()
			return nil
		},
	}
}
