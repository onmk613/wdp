package executor

// RunPlan：执行 plan（P1 的 apply 路径，P2 的 agent 自治执行复用同一入口）。
//
// plan 内嵌了完整的 chart 树快照（编译期逐文件固化）。RunPlan 把它物化到
// 临时目录后走 chart.Load 的常规加载路径，再以"冻结变量域"模式驱动既有
// 执行器——与 run 路径共用 runTaskOnHost 以下的全部代码，语义零漂移：
//
//   - 跨主机信息（groups/hosts/hostvars/values/play vars）取编译期冻结值
//     （HostPlan.Vars），主机侧执行时拿不到其他主机的 facts；
//   - 单主机运行时信息（register/set_fact/setup 采集、when/loop/模板渲染、
//     play_batch）仍在本机求值；
//   - chart: 引用按计划内 chart 树在执行侧展开（展开语义单一实现）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"wdp/internal/chart"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/plan"
	"wdp/internal/render"
)

// RunPlan 执行已批准的计划。返回是否存在失败主机。plan 物化失败按整体
// 失败处理（best-effort 播报后返回 true）。
func (e *Executor) RunPlan(ctx context.Context, p *plan.Plan) bool {
	tmp, err := os.MkdirTemp("", "wdp-plan-")
	if err != nil {
		e.Rep.PlayMsg("plan materialize failed: %v", err)
		return true
	}
	defer os.RemoveAll(tmp)

	root, err := p.Materialize(tmp)
	if err != nil {
		e.Rep.PlayMsg("plan materialize failed: %v", err)
		return true
	}
	// 大文件引用经 --chart-dir 补齐（校验摘要；缺失仅告警——在线模式可由
	// 模块自行按 URL 获取）
	if err := e.supplyPayloads(p, root); err != nil {
		e.Rep.PlayMsg("plan payloads: %v", err)
	}
	ch, err := chart.LoadWithLimits(root, chart.Limits{})
	if err != nil {
		e.Rep.PlayMsg("plan chart load failed: %v", err)
		return true
	}
	defer ch.Close()
	eng, err := render.NewEngine(ch.CollectHelpers())
	if err != nil {
		e.Rep.PlayMsg("plan chart helpers invalid: %v", err)
		return true
	}

	// 计划内主机清单 → 合成 inventory（连接元数据来自计划）。hosts 模式
	// 不能一律 all：编译期是逐 play 选主机（plan/compile.go），一个 chart
	// 的多个 play 往往作用于不同主机组，全 all 会让后续 play 跑到不属于
	// 它的主机上。每个 play 用一个只含自己成员的合成组，见 playsOf。
	var hosts []*model.Host
	seen := map[string]bool{}
	for _, name := range p.Host() {
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, hp := range p.HostPlansOf(name) {
			hosts = append(hosts, hp.Conn.Host(name))
			break
		}
	}
	// 冻结变量域按 (playIdx, host) 索引：同一主机出现在多个 play 时，
	// 各 play 的 play vars/play_hosts 快照不同，不能以主机名为键互相覆盖。
	hostValues := make(map[string]map[string]any, len(p.Hosts))
	planVars := make(map[string]map[string]any, len(p.Hosts))
	for _, hp := range p.Hosts {
		hostValues[planScopeKey(hp.PlayIdx, hp.Host)] = hp.Values
		planVars[planScopeKey(hp.PlayIdx, hp.Host)] = hp.Vars
	}

	byName := make(map[string]*model.Host, len(hosts))
	for _, h := range hosts {
		byName[h.Name] = h
	}
	e.Inv = inventory.FromHosts(hosts)
	e.Inv.Groups = planPlayGroups(p, byName) // 每个 play 的合成组（playsOf 以组名选主机）
	e.Opts.Chart = ch
	e.Opts.BaseDir = root
	e.Opts.Values = p.Values
	e.Opts.HostValues = hostValues
	e.Opts.PlanVars = planVars
	e.Opts.Phase = p.Phase
	e.engine = eng

	return e.Run(ctx, playsOf(p))
}

// supplyPayloads 从 PayloadDir 补齐 plan 的大文件引用（摘要校验，防止
// 版本错配的制品被静默使用）。PayloadDir 未配置或文件缺失时跳过（告警）。
func (e *Executor) supplyPayloads(p *plan.Plan, root string) error {
	if e.Opts.PayloadDir == "" || len(p.Payloads) == 0 {
		return nil
	}
	for _, ref := range p.Payloads {
		src := filepath.Join(e.Opts.PayloadDir, ref.Path)
		sum, err := fileSHA256Path(src)
		if err != nil {
			e.Rep.PlayMsg("payload %s unavailable via --chart-dir (%v); module will fall back to its own source", ref.Path, err)
			continue
		}
		if sum != ref.SHA256 {
			return fmt.Errorf("payload %s sha256 mismatch: --chart-dir has %s, plan expects %s", ref.Path, sum, ref.SHA256)
		}
		dst := filepath.Join(root, ref.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// playsOf 把计划的扁平 (play, host) 分片重建为 play 编排：同 PlayIdx 的
// 分片合成一个 play（任务清单在编译期由同一 play 产出，取首条即可），
// pre/post hook 任务按原 Hook 标记拼回主列表——runPlay 的 hook 拆分是
// 幂等的，重建后再拆结果一致。
//
// Hosts 取该 play 的合成组名而非 "all"：编译期是逐 play 选主机的，多 play
// chart 的各 play 常作用于不同主机组，"all" 会把后续 play 下发到不属于它的
// 主机（合成 inventory 是全 plan 主机的并集）。
func playsOf(p *plan.Plan) []*model.Play {
	var idxs []int
	byIdx := map[int]*plan.HostPlan{}
	for _, hp := range p.Hosts {
		if _, ok := byIdx[hp.PlayIdx]; !ok {
			idxs = append(idxs, hp.PlayIdx)
		}
		byIdx[hp.PlayIdx] = hp // 首条为该 play 的代表分片
	}
	var plays []*model.Play
	for _, idx := range idxs {
		hp := byIdx[idx]
		play := &model.Play{
			Name:        hp.Play.Name,
			Hosts:       planPlayGroupName(idx),
			PlanIdx:     idx,
			Become:      hp.Play.Become,
			BecomeUser:  hp.Play.BecomeUser,
			Serial:      hp.Play.Serial,
			Strategy:    hp.Play.Strategy,
			Environment: hp.Play.Environment,
		}
		for _, group := range [][]*plan.ResolvedTask{hp.Pre, hp.Tasks, hp.Post} {
			for _, rt := range group {
				play.Tasks = append(play.Tasks, taskOf(rt))
			}
		}
		for _, rt := range hp.Handlers {
			h := taskOf(rt)
			h.IsHandler = true
			play.Handlers = append(play.Handlers, h)
		}
		plays = append(plays, play)
	}
	return plays
}

// planPlayGroupName 返回 play 的合成组名。合成 inventory 里不存在真实组，
// 该名字只用于"按编译期固化的主机集合选主机"。
func planPlayGroupName(playIdx int) string {
	return fmt.Sprintf("__wdp_play_%d", playIdx)
}

// planScopeKey 是 plan 执行模式下"每主机 values / 冻结变量域"的键。同一
// 主机可能出现在多个 play（各 play 的 play vars 与 play_hosts 快照不同），
// 仅以主机名为键会让后一个 play 的冻结值覆盖前一个。
func planScopeKey(playIdx int, host string) string {
	return strconv.Itoa(playIdx) + "|" + host
}

// planPlayGroups 为每个 play 构造只含其成员主机的合成组（成员指针取自
// 合成 inventory，保证与连接元数据是同一对象）。
func planPlayGroups(p *plan.Plan, byName map[string]*model.Host) map[string]*model.Group {
	groups := map[string]*model.Group{}
	seen := map[int]map[string]bool{}
	for _, hp := range p.Hosts {
		name := planPlayGroupName(hp.PlayIdx)
		g := groups[name]
		if g == nil {
			g = &model.Group{Name: name}
			groups[name] = g
			seen[hp.PlayIdx] = map[string]bool{}
		}
		if seen[hp.PlayIdx][hp.Host] {
			continue
		}
		seen[hp.PlayIdx][hp.Host] = true
		if h := byName[hp.Host]; h != nil {
			g.Hosts = append(g.Hosts, h)
			g.HostNames = append(g.HostNames, hp.Host)
		}
	}
	return groups
}

// fileSHA256Path 计算文件摘要（不存在时返回错误）。
func fileSHA256Path(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile 拷贝文件内容（0600，制品不落明文权限）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// taskOf 把计划任务镜像还原为执行任务（Label 还原为 Name：与模块名相同
// 的标签是缺省名，不回填）。
func taskOf(rt *plan.ResolvedTask) *model.Task {
	t := &model.Task{
		PlanIdx:      rt.Idx,
		Module:       rt.Module,
		ChartRef:     rt.ChartRef,
		TasksFrom:    rt.TasksFrom,
		ChartVars:    rt.ChartVars,
		Args:         rt.Args,
		FreeForm:     rt.FreeForm,
		When:         rt.When,
		Loop:         rt.Loop,
		LoopVar:      rt.LoopVar,
		Register:     rt.Register,
		Notify:       rt.Notify,
		Tags:         rt.Tags,
		Environment:  rt.Environment,
		IgnoreErrors: rt.IgnoreErrors,
		Retries:      rt.Retries,
		DelaySec:     rt.DelaySec,
		TimeoutSec:   rt.TimeoutSec,
		Become:       rt.Become,
		BecomeUser:   rt.BecomeUser,
		ChangedWhen:  rt.ChangedWhen,
		FailedWhen:   rt.FailedWhen,
		Until:        rt.Until,
		Output:       rt.Output,
		NoLog:        rt.NoLog,
		DelegateTo:   rt.DelegateTo,
		RunOnce:      rt.RunOnce,
		Hook:         rt.Hook,
		IsHandler:    false,
	}
	if rt.Label != "" && rt.Label != rt.Module {
		t.Name = rt.Label
	}
	for _, b := range rt.Block {
		t.Block = append(t.Block, taskOf(b))
	}
	for _, b := range rt.Rescue {
		t.Rescue = append(t.Rescue, taskOf(b))
	}
	for _, b := range rt.Always {
		t.Always = append(t.Always, taskOf(b))
	}
	return t
}
