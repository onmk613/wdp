package executor

import (
	"context"
	"fmt"
	"strings"

	"wdp/internal/chart"
	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

// writeMarkers 部署成功后写 release marker（best-effort：失败仅告警不中断）。
func (e *Executor) writeMarkers(ctx context.Context, hosts []*model.Host, ch *chart.Chart) {
	if !ch.MarkerEnabled() {
		return
	}
	path := ch.MarkerPath()
	content := ch.MarkerContent(e.Opts.WdpVersion, e.Opts.Values, e.Opts.Phase)
	written := 0
	for _, h := range hosts {
		cn, err := e.Conns.Get(ctx, h)
		if err != nil {
			continue
		}
		if out, bad := cn.Exec(ctx, conn.ExecRequest{
			Script: fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(pathDir(path))), TimeoutMs: 10_000,
		}); bad != nil || out.Code != 0 {
			continue
		}
		if err := cn.UploadFile(ctx, path, strings.NewReader(string(content)), 0o600); err == nil {
			written++
		}
	}
	if written > 0 {
		e.Rep.PlayMsg("release marker written to %d hosts: %s", written, path)
	} else if len(hosts) > 0 {
		e.Rep.PlayMsg("warning: release marker write failed (uninstall/status will be unavailable): %s", path)
	}
}

// removeMarkers 卸载成功后清除 release marker。只删 marker 文件本身，
// 目录用 rmdir 收敛（仅当为空时生效）——绝不对 <marker_dir>/<name> 整体
// rm -rf：name/marker_dir 已在加载入口校验，此处再按"仅文件"收紧，
// 双重防线避免目录内混入无关文件时被连带删除。
func (e *Executor) removeMarkers(ctx context.Context, hosts []*model.Host, ch *chart.Chart) {
	if !ch.MarkerEnabled() {
		return // no_marker chart 从未写 marker，卸载不应执行 rm/rmdir
	}
	marker := ch.MarkerPath()
	script := fmt.Sprintf("rm -f -- %s; rmdir -- %s 2>/dev/null; [ ! -e %s ]",
		shellquote.Quote(marker), shellquote.Quote(pathDir(marker)), shellquote.Quote(marker))
	done := 0
	for _, h := range hosts {
		cn, err := e.Conns.Get(ctx, h)
		if err != nil {
			continue
		}
		if out, bad := cn.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 10_000}); bad == nil && out.Code == 0 {
			done++
		}
	}
	if done > 0 {
		e.Rep.PlayMsg("release marker removed from %d hosts", done)
	}
}
