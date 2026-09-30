package web

// 保留策略循环：runs/run_tasks/audit_logs/app_drafts 的时间清理通道。
// 此前只有 metrics_5m 按 30 天清桶——执行明细（stdout/stderr/脚本快照）
// 与审计随历史线性增长，库无界膨胀；草稿 payload 含口令，弃置即残留。
// 每日一轮，启动 1 分钟后先跑一轮（避开启动关键路径）。

import (
	"context"
	"time"
)

// startRetentionLoop 阻塞执行保留策略循环（Run 里以 goroutine 启动）。
func (s *Server) startRetentionLoop(ctx context.Context) {
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	s.retentionOnce()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.retentionOnce()
		}
	}
}

// retentionOnce 执行一轮清理（0 天 = 对应表永久保留）。
func (s *Server) retentionOnce() {
	if d := s.runsRetentionDays(); d > 0 {
		if n, err := s.st.PruneRuns(retentionCutoff(d)); err != nil {
			s.logger.Warn("retention: prune runs failed", "err", err)
		} else if n > 0 {
			s.logger.Info("retention: pruned runs", "n", n, "days", d)
		}
	}
	if d := s.auditRetentionDays(); d > 0 {
		if n, err := s.st.PruneAuditLogs(retentionCutoff(d)); err != nil {
			s.logger.Warn("retention: prune audit logs failed", "err", err)
		} else if n > 0 {
			s.logger.Info("retention: pruned audit logs", "n", n, "days", d)
		}
	}
	if d := s.draftsRetentionDays(); d > 0 {
		if n, err := s.st.PruneAppDrafts(retentionCutoff(d)); err != nil {
			s.logger.Warn("retention: prune drafts failed", "err", err)
		} else if n > 0 {
			s.logger.Info("retention: pruned app drafts", "n", n, "days", d)
		}
	}
}

// retentionCutoff 与 nowUTC 同格式（RFC3339 UTC）：落库时间串的字节序
// 比较即时间序比较。
func retentionCutoff(days int) string {
	return time.Now().Add(-time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)
}
