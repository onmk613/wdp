package agent

// 执行主体在 internal/selfrun（HTTP /exec 与 conn/selfexec 共用的单一实现，
// 防两份提权语义漂移）。此处保留兼容别名。

import "wdp/internal/selfrun"

// ExecReq 是一次脚本执行请求（HTTP /exec 与自治执行共用）。
type ExecReq = selfrun.ExecReq

// ExecResp 是脚本执行结果。
type ExecResp = selfrun.ExecResp

// RunScript 在 agent 进程所在主机上执行一次脚本（selfrun 的单一实现）。
var RunScript = selfrun.RunScript
