// Package worker 承载控制台的后台周期任务（与 HTTP 传输解耦）：
//
//   - Prober：全量主机探活（状态写回台账）；
//   - Monitor：agent /metrics 周期采样 → 派生指标 → 5 分钟桶 → 阈值告警。
//
// 依赖全部由构造方注入（store/logger/回调），不 import web——传输层的
// mTLS 客户端与 scheme 选择留在 web，经 Fetch/Probe 回调传入。Run 挂在
// server 生命周期 ctx 上，取消即退出。
package worker
