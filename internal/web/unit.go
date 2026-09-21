package web

// wdp agent 的 systemd 单元装配公共件：enroll 下发脚本、SSH 推装、远程
// 升级三处引用同一形态——unit 内容与单元名必须一致，否则重装或升级会以
// 不同形态覆盖对方的 unit 文件（Restart 策略漂移一类问题难排查）。

import "fmt"

// agentUnitName agent 的 systemd 单元名（不含 .service 后缀）。
const agentUnitName = "wdp-agent"

// agentUnitFile 生成 unit 文件内容。binDir/etcDir/logFile 传 shell 变量
// 引用（$BIN_DIR 等）时供 enroll 脚本 heredoc 内展开（变量在脚本头部单点
// 定义）；传字面路径时即目标机落盘内容（SSH 推装直接写文件）。
func agentUnitFile(binDir, etcDir, logFile string, port int) string {
	return fmt.Sprintf(`[Unit]
Description=wdp agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s/wdp agent --listen 0.0.0.0:%d --ca %s/ca.crt --cert %s/agent.crt --key %s/agent.key --log-file %s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, binDir, port, etcDir, etcDir, etcDir, logFile)
}
