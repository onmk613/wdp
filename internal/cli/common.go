package cli

// 命令装配的公共胶水：conn 驱动注册、跨命令的 flag/补全声明、主机来源
// 解析（-i 清单 vs --hosts 内联）与 config → 连接/执行层的桥接取值。

import (
	"fmt"

	"github.com/spf13/cobra"

	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/inventory"

	// 连接驱动注册（副作用导入：各驱动在 init 里向 conn 注册 factory，
	// 本包任何走 conn.NewManager* 的命令都依赖这里的注册）
	_ "wdp/internal/conn/agentc"
	_ "wdp/internal/conn/local"
	_ "wdp/internal/conn/push"
	_ "wdp/internal/conn/sshc"
)

// errPlayFailed 标记存在失败主机（退出码 1，不打印重复错误）。
var errPlayFailed = fmt.Errorf("execution finished with failed hosts")

// chartValueFlags 声明 chart 公共 flag（-f/--values/--set）。
func chartValueFlags(cmd *cobra.Command, valuesFiles, setArgs *[]string) {
	cmd.Flags().StringArrayVarP(valuesFiles, "values-file", "f", nil,
		"chart values override files (repeatable, deep-merged in order, like Helm)")
	cmd.Flags().StringArrayVar(setArgs, "set", nil,
		"chart values dot-path overrides (--set a.b[0]=v, repeatable)",
	)
}

// completePathArgs 供"位置参数是本地路径 + 命令又带子命令"的命令（plan、
// apply）恢复 shell 默认文件补全：cobra 对有子命令的命令默认返回
// NoFileComp，本地 chart/plan 路径便无法补全；返回 Default 让 shell 回落
// 到自身文件补全，子命令名仍由 cobra 一并给出（两者共存）。
func completePathArgs(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveDefault
}

// shortID 截断 sha256 等长 ID 供展示（plan_id / 自治 run_id）。
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// hostSource 解析主机来源：--hosts 内联表达式（临时目标免 inventory
// 文件）或 -i 清单。第二返回值标记是否内联模式（此时 play 的主机模式
// 一律改写为 all，全部内联主机都是目标）。
func hostSource(hostsInline []string) (*inventory.Inventory, bool, error) {
	if len(hostsInline) == 0 {
		inv, err := loadInventories()
		return inv, false, err
	}
	// 只与"用户显式 -i"互斥；gInventories 可能是 PersistentPreRunE 回填的
	// config 默认值，不代表用户指定了清单
	if gInventoryExplicit {
		return nil, false, fmt.Errorf("--hosts and -i are mutually exclusive (inline specs replace the inventory file)")
	}
	hs, err := inventory.HostsFromSpecs(hostsInline, connDefaults())
	if err != nil {
		return nil, false, err
	}
	if len(hs) == 0 {
		return nil, false, fmt.Errorf("--hosts resolved to no hosts")
	}
	return inventory.FromHosts(hs), true, nil
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
		Conn:                c.DefaultConn(),
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
