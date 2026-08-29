package cli

import (
	"github.com/spf13/cobra"

	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/inventory"
)

// chartValueFlags 声明 chart 公共 flag（-f/--values/--set）。
func chartValueFlags(cmd *cobra.Command, valuesFiles, setArgs *[]string) {
	cmd.Flags().StringArrayVarP(valuesFiles, "values-file", "f", nil,
		"chart values override files (repeatable, deep-merged in order, like Helm)")
	cmd.Flags().StringArrayVar(setArgs, "set", nil,
		"chart values dot-path overrides (--set a.b[0]=v, repeatable)",
	)
}

// loadInventories 加载全部 -i 清单
func loadInventories() (*inventory.Inventory, error) {
	paths := gInventories
	if len(paths) == 0 {
		paths = []string{"inventory.yaml"}
	}
	return inventory.LoadMergeWithConfig(paths, config.Current())
}

// connDefaults 从 wdp.cfg 归一出连接层默认值（组合根显式注入，
// 连接层自身不依赖 config 包）。归一化职责划分：SSH 用户/超时在 config
// 取值器归一（inventory 烘焙 host 字段共用）；agent 类默认值注入原始值、
// 由 conn.Defaults 的 OrDefault 系列归一（conn 层是唯一消费方）。
func connDefaults() *conn.Defaults {
	c := config.Current()
	return &conn.Defaults{
		SSHUser:             c.SSHUser(),
		SSHConnectTimeout:   c.SSHConnectTimeout(),
		AgentPort:           c.Agent.Port,
		AgentCertRotateMin:  c.AgentCertRotateMin(),
		PushCADir:           c.Agent.PushCADir,
		AgentIdleTimeoutMin: c.AgentIdleTimeoutMin(),
		PushBinary:          c.Agent.PushBinary,
	}
}

// maxDownloadBytes 归一 get_url 下载上限（--max-download-mb > wdp.cfg > 内置默认；
// flag 覆盖已在 PersistentPreRunE 写入 config，0 表示用模块内置默认）。
func maxDownloadBytes() int64 {
	if mb := config.Current().Transfer.MaxDownloadMB; mb > 0 {
		return int64(mb) << 20
	}
	return 0
}
