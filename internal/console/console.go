// Package console 承载控制台的领域动作（应用 spec 的物化/校验/打包入库）：
// 签名不碰 ResponseWriter，HTTP 语义（状态码/审计/权限）留在 web 传输层。
//
// 分层约定：web → console → store。新功能先落这里，web 只做解码、权限、
// 调用与响应；存量 handler 按文件逐步迁移（行为零变化）。
package console

import (
	"log/slog"
	"path/filepath"

	"wdp/internal/store"
)

// AppService 是应用（chart）的领域服务：spec 物化、工作目录准备、打包
// 归位。一个 server 实例一份；全部方法无共享可变状态（文件系统操作的
// 互斥由调用方的事务性临界区保证，与此前 web.uploadMu 语义一致）。
type AppService struct {
	Store   *store.Store
	DataDir string
	Logger  *slog.Logger
}

// AppsDir 应用制品根目录。
func (a *AppService) AppsDir() string {
	if a.DataDir == "" {
		return "wdp-data/apps"
	}
	return filepath.Join(a.DataDir, "apps")
}
