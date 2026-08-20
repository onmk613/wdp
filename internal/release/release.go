// Package release 记录每次 chart 部署（chart 版本 + values 快照 + 结果），
// 供 wdp release list / show 审计与重放（show --values 输出可直接 -f 复用）。
package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wdp/internal/model"
)

// Record 是一次部署记录。
type Record struct {
	ID        string                  `json:"id"` // <chart>-<unix>
	Time      time.Time               `json:"time"`
	Chart     string                  `json:"chart,omitempty"`
	Version   string                  `json:"version,omitempty"`
	Playbook  string                  `json:"playbook,omitempty"` // 裸 playbook 模式
	Values    map[string]any          `json:"values,omitempty"`
	ValuesRef []string                `json:"values_ref,omitempty"` // -f 文件与 --set 明细
	Hosts     []string                `json:"hosts"`
	Stats     map[string]*model.Stats `json:"stats"`
	Failed    bool                    `json:"failed"`
}

// Dir 返回记录目录（~/.wdp/releases）。
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".wdp", "releases"), nil
}

// Save 写入一条记录，返回 ID。
func Save(rec *Record) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := rec.Chart
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(rec.Playbook), filepath.Ext(rec.Playbook))
	}
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	rec.ID = fmt.Sprintf("%s-%d", name, rec.Time.Unix())
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	return rec.ID, os.WriteFile(filepath.Join(dir, rec.ID+".json"), data, 0o600)
}

// List 列出记录（新在前），chartFilter 非空时按 chart 名前缀过滤。
func List(chartFilter string) ([]*Record, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, err := Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		if chartFilter != "" && !strings.HasPrefix(rec.ID, chartFilter) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// Load 读取一条记录。
func Load(id string) (*Record, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("记录 %s 不存在", id)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}
