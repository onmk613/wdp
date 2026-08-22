// Package release 记录每次 chart 部署（chart 版本 + values 快照 + 结果），
// 供部署审计与重放（values 快照输出可直接作为 -f 输入复用）。
package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cyphar/filepath-securejoin"

	"wdp/internal/i18n"
	"wdp/internal/model"
)

// Record 是一次部署记录。
type Record struct {
	ID        string                  `json:"id"` // <chart>-<unixnano>
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
	// 纳秒粒度 ID：秒级粒度下同名 chart 同秒并发部署会互相覆盖审计记录；
	// 配合 O_EXCL 创建，冲突（同纳秒）时追加序号重试
	rec.ID = fmt.Sprintf("%s-%d", name, rec.Time.UnixNano())
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	// 记录路径经 securejoin 约束在记录目录内: ID 源自 chart 名,
	// 含 ../ 的名字被收敛为目录内路径, 不会越出 releases 写文件.
	for attempt := 0; ; attempt++ {
		id := rec.ID
		if attempt > 0 {
			id = fmt.Sprintf("%s-%d", rec.ID, attempt)
		}
		path, err := securejoin.SecureJoin(dir, id+".json")
		if err != nil {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if os.IsExist(err) {
			continue // 极小概率的同名冲突，追加序号重试
		}
		if err != nil {
			return "", err
		}
		// 完整写入后无原子性问题（O_EXCL 新文件 + 单次写入），fsync 防宕机截断
		if _, werr := f.Write(data); werr != nil {
			f.Close()
			return "", werr
		}
		if werr := f.Sync(); werr != nil {
			f.Close()
			return "", werr
		}
		if werr := f.Close(); werr != nil {
			return "", werr
		}
		rec.ID = id
		return id, nil
	}
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

// Load 读取一条记录。id 经 securejoin 约束在记录目录内（../ 等穿越
// 写法不越出 releases 目录读文件）。
func Load(id string) (*Record, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	path, err := securejoin.SecureJoin(dir, id+".json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(i18n.T("record %s does not exist", "记录 %s 不存在"), id)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// DiffValues 递归对比两份 values，返回 "- 路径: 旧值 / + 路径: 新值" 变更行
// （新增/删除项带标注；无差异返回空）。部署对比功能用其对比两次部署的快照。
func DiffValues(a, b map[string]any) []string {
	var lines []string
	diffValues("", a, b, &lines)
	return lines
}

// diffValues 递归对比两份 values，输出 - 路径: 旧值 / + 路径: 新值 行。
func diffValues(prefix string, a, b map[string]any, out *[]string) {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		av, aok := a[k]
		bv, bok := b[k]
		switch {
		case !aok:
			*out = append(*out, fmt.Sprintf(i18n.T("+ %s: %v (added)", "+ %s: %v（新增）"), path, bv))
		case !bok:
			*out = append(*out, fmt.Sprintf(i18n.T("- %s: %v (removed)", "- %s: %v（删除）"), path, av))
		default:
			am, aIsMap := av.(map[string]any)
			bm, bIsMap := bv.(map[string]any)
			if aIsMap && bIsMap {
				diffValues(path, am, bm, out)
				continue
			}
			if fmt.Sprint(av) != fmt.Sprint(bv) {
				*out = append(*out, fmt.Sprintf("- %s: %v", path, av))
				*out = append(*out, fmt.Sprintf("+ %s: %v", path, bv))
			}
		}
	}
	// 输出按路径排序：map 迭代顺序随机会让同一对记录的 diff 行序每次不同，
	// 无法用于稳定的回归对比
	sort.Slice(*out, func(i, j int) bool {
		pi := strings.Fields((*out)[i])
		pj := strings.Fields((*out)[j])
		if len(pi) > 1 && len(pj) > 1 {
			return pi[1] < pj[1]
		}
		return (*out)[i] < (*out)[j]
	})
}
