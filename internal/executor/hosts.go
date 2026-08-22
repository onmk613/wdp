package executor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// 主机选择、批次切分与存活跟踪。

func (e *Executor) selectHosts(pattern string) ([]*model.Host, error) {
	hosts, err := e.Inv.Select(pattern)
	if err != nil {
		return nil, err
	}
	if e.Opts.Limit == "" {
		return e.filterDead(hosts), nil
	}
	limited, err := e.Inv.Select(e.Opts.Limit)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, h := range limited {
		set[h.Name] = true
	}
	out := []*model.Host{}
	for _, h := range hosts {
		if set[h.Name] {
			out = append(out, h)
		}
	}
	return e.filterDead(out), nil
}

// filterDead 剔除本次 run 内已失败/不可达的主机（后续 play 不再参与，
// 避免在安装失败的节点上继续执行启动类 play）。
func (e *Executor) filterDead(hosts []*model.Host) []*model.Host {
	e.deadMu.Lock()
	defer e.deadMu.Unlock()
	if len(e.deadHosts) == 0 {
		return hosts
	}
	out := hosts[:0:0]
	for _, h := range hosts {
		if e.deadHosts[h.Name] {
			e.Rep.PlayMsg("%s 此前失败，跳过本 play", h.Name)
			continue
		}
		out = append(out, h)
	}
	return out
}

// markDead 登记不可继续参与后续 play 的主机。
func (e *Executor) markDead(host string) {
	e.deadMu.Lock()
	e.deadHosts[host] = true
	e.deadMu.Unlock()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// splitBatches 按 serial 表达式分批："5"（每批 5 台）/"10%"（百分比）/
// "5,10,20"（逐批尺寸，最后一个尺寸对剩余主机重复）；空 = 一批。
// 表达式含空段（如 "5," 笔误）时报错，而不是静默回退默认分批。
func splitBatches(hosts []*model.Host, serial string) ([][]*model.Host, error) {
	if serial == "" {
		return [][]*model.Host{hosts}, nil
	}
	var sizes []int
	for _, t := range strings.Split(serial, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			return nil, fmt.Errorf(i18n.T("invalid serial %q: empty segment (check for stray commas)", "serial %q 非法：存在空段（检查多余逗号）"), serial)
		}
		sizes = append(sizes, parseBatchSize(t, len(hosts)))
	}
	if len(sizes) == 0 {
		return [][]*model.Host{hosts}, nil
	}
	var out [][]*model.Host
	for i := 0; i < len(hosts); {
		size := sizes[len(sizes)-1] // 最后一个尺寸对剩余主机重复使用
		if len(out) < len(sizes) {
			size = sizes[len(out)]
		}
		end := i + size
		if end > len(hosts) {
			end = len(hosts)
		}
		out = append(out, hosts[i:end])
		i = end
	}
	return out, nil
}

func anyAlive(runs []*hostRun) bool {
	for _, hr := range runs {
		if hr.alive {
			return true
		}
	}
	return false
}

func hostNames(hosts []*model.Host) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return out
}

// randSuffix 生成不可预测的临时路径后缀（回滚快照目录），
// 避免 UnixNano 可预测路径在远端 /tmp 被预创建符号链接劫持。
func randSuffix() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// localhost 返回 delegate_to: localhost 的本地执行主机（每次新建，避免共享可变结构）。
func (e *Executor) localhost() *model.Host { return &model.Host{Name: "localhost", Conn: "local"} }
