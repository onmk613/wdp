package agentops

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"wdp/internal/ca"
	"wdp/internal/conn"
	"wdp/internal/conn/sshc"
	"wdp/internal/model"
)

// RenewCert 逐主机延期+推送+重启：本地更新延期（增量+备份，复用 wdp ca
// renew 语义）→ SSH 推送新证书 → 重启 agent → 可选验证。
func RenewCert(ctx context.Context, hosts []*model.Host, o Options, forks int, dc *conn.Defaults, out io.Writer) error {
	if err := validateOptions(o); err != nil {
		return err
	}
	failed := forEachHost(ctx, hosts, forks, func(ctx context.Context, h *model.Host) error {
		identity, err := certIdentity(h)
		if err != nil {
			return err
		}
		crt := filepath.Join(o.CertDir, identity+".crt")
		// 本地更新延期：同路径原地（自动备份 *.old.<ts>），身份字段全继承
		newCrt, newKey, _, err := ca.Renew(ca.RenewOptions{
			CertPath: crt, KeyPath: filepath.Join(o.CertDir, identity+".key"),
			OutPath: crt, CACertPath: o.CACertPath, CAKeyPath: o.CAKeyPath,
			NewKey: o.NewKey, Days: o.Days,
		})
		if err != nil {
			return fmt.Errorf("renew %s: %w", identity, err)
		}
		ssh := sshc.New(h)
		if err := ssh.Connect(ctx); err != nil {
			return err
		}
		defer ssh.Close()
		if err := uploadTriple(ctx, ssh, o.RemoteDir, newCrt, newKey, o.CACertPath); err != nil {
			return err
		}
		if err := sshRun(ctx, ssh, "systemctl restart "+quoteSh(o.UnitName)); err != nil {
			return fmt.Errorf("restart agent: %w", err)
		}
		if o.ClientCert != "" {
			port := h.AgentPort
			if port == 0 {
				port = dc.AgentPortOrDefault() // h.AgentPort > wdp.cfg [agent].port > 7602
			}
			// systemd 重启后短暂等待监听就绪
			time.Sleep(300 * time.Millisecond)
			if err := verifyAgent(ctx, h, port, o, dc); err != nil {
				return fmt.Errorf("renewed but verification failed: %w", err)
			}
		}
		fmt.Fprintf(out, "%s: renewed (+%dd)\n", h.Name, o.Days)
		return nil
	})
	return summarize(failed, len(hosts))
}
