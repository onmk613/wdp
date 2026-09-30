package store

import (
	_ "modernc.org/sqlite"
)

// ---- 监控指标（5 分钟聚合）与主机健康告警 ----
// 单样本写入统一走 BatchUpsertMetric5m（单事务批量）；逐条自提交的
// UpsertMetric5m 无任何调用方，已删除。

// MetricPoint 是一批同主机同桶的样本（批量写用）。
type MetricPoint struct {
	Metric, Labels string
	V              float64
}

// BatchUpsertMetric5m 一台主机一轮采样并入桶（单事务——scrapeLoop 每分钟
// 每主机十几条样本，逐条自提交事务的 fsync 开销是监控写入的主要成本）。
func (s *Store) BatchUpsertMetric5m(hostID int64, bucket int64, pts []MetricPoint) error {
	tx, err := s.raw.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO metrics_5m (host_id, metric, labels, bucket, n, vsum, vmax)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(host_id, metric, labels, bucket)
		DO UPDATE SET n = n + 1, vsum = vsum + excluded.vsum, vmax = max(vmax, excluded.vmax)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, p := range pts {
		if _, err := stmt.Exec(hostID, p.Metric, p.Labels, bucket, p.V, p.V); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// SeriesPoint 是一个桶的聚合值。
type SeriesPoint struct {
	Ts  int64   `json:"ts"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// QuerySeries 查某主机某指标（可带 labels 精确匹配）自 from 起的桶序列。
func (s *Store) QuerySeries(hostID int64, metric, labels string, from int64) ([]*SeriesPoint, error) {
	rows, err := s.query(`SELECT bucket, vsum / n, vmax FROM metrics_5m
		WHERE host_id = ? AND metric = ? AND labels = ? AND bucket >= ? ORDER BY bucket`,
		hostID, metric, labels, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SeriesPoint{}
	for rows.Next() {
		p := &SeriesPoint{}
		if err := rows.Scan(&p.Ts, &p.Avg, &p.Max); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneMetrics 清理过期桶（保留窗口外全删）。
func (s *Store) PruneMetrics(before int64) error {
	_, err := s.exec(`DELETE FROM metrics_5m WHERE bucket < ?`, before)
	return err
}

// HostAlert 是一条主机健康告警（页面标记用；真正的告警走 Prometheus）。
type HostAlert struct {
	HostID    int64   `json:"HostID"`
	HostName  string  `json:"HostName"`
	Kind      string  `json:"Kind"`  // cpu / mem / fs / offline
	Level     string  `json:"Level"` // warn / crit
	Detail    string  `json:"Detail"`
	Value     float64 `json:"Value"`
	UpdatedAt string  `json:"UpdatedAt"`
}

// SetHostAlert upsert 一条告警。
func (s *Store) SetHostAlert(hostID int64, kind, level, detail string, value float64) error {
	_, err := s.exec(`INSERT INTO host_alerts (host_id, kind, level, detail, value, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(host_id, kind) DO UPDATE SET level = excluded.level, detail = excluded.detail,
		value = excluded.value, updated_at = excluded.updated_at`,
		hostID, kind, level, detail, value, nowUTC())
	return err
}

// ClearHostAlert 删除指定告警（指标恢复正常时）。
func (s *Store) ClearHostAlert(hostID int64, kinds ...string) error {
	// 多条删除同事务：半途失败会出现部分已恢复、部分仍告警的分裂状态
	return s.tx(func(q execer) error {
		for _, k := range kinds {
			if _, err := q.Exec(`DELETE FROM host_alerts WHERE host_id = ? AND kind = ?`, hostID, k); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListHostAlerts 当前全部告警（带主机名）。
func (s *Store) ListHostAlerts() ([]*HostAlert, error) {
	rows, err := s.query(`SELECT ha.host_id, COALESCE(h.name, ''), ha.kind, ha.level, ha.detail, ha.value, ha.updated_at
		FROM host_alerts ha LEFT JOIN hosts h ON h.id = ha.host_id ORDER BY ha.host_id, ha.kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*HostAlert{}
	for rows.Next() {
		a := &HostAlert{}
		if err := rows.Scan(&a.HostID, &a.HostName, &a.Kind, &a.Level, &a.Detail, &a.Value, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
