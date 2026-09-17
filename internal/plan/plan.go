// Package plan 定义 wdp 的执行计划（plan）产物：控制端离线编译出的、
// 完全解析的部署意图。plan 是"跨主机信息已固化、单主机运行时信息保留"
// 的快照——groups/hosts/hostvars 在编译期固化为字面值（主机侧拿不到其他
// 主机的 facts，见 docs/15 §2.3），when/loop/模板渲染留给执行侧（它们可
// 能依赖运行时 register）。
//
// 确定性是 plan 的立身之本：同一 chart + 同一 values 两次编译必须产出
// 逐字节相同的 plan.json——这是 PlanID 内容寻址与 plan diff 成立的前提。
// 因此 plan 不含时间戳，JSON 键序由 encoding/json 的 map 排序保证。
package plan

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"wdp/internal/chart"
	"wdp/internal/model"
)

// SchemaVer 是当前 plan 结构版本。
const SchemaVer = 1

// 文件嵌入上限：chart 是配置载体（模板/清单/小文件），大负载走 artifact
// 模块按 URL 分发——超限即编译报错，避免 plan 变成制品分发通道。
const (
	MaxFileBytes  int64 = 32 << 20  // 单文件 32MiB
	MaxTotalBytes int64 = 256 << 20 // 全部文件合计 256MiB
)

// 阈值以变量形式参与判定：测试用小型临时文件即可覆盖"超限转 payload /
// 总量超限报错"两条分支，无需真的写出 32MiB 制品。生产路径恒等于上面的常量。
var (
	maxFileBytes  = MaxFileBytes
	maxTotalBytes = MaxTotalBytes
)

// Plan 是一次执行的完全解析产物。
type Plan struct {
	SchemaVer  int               `json:"schema"`      // 结构版本（当前 1）
	PlanID     string            `json:"plan_id"`     // 内容寻址：sha256(canonical(Plan))，不含自身
	Chart      string            `json:"chart"`       // chart 名
	Version    string            `json:"version"`     // chart 版本
	Phase      string            `json:"phase"`       // 生命周期相位（空串按 deploy）
	WdpVersion string            `json:"wdp_version"` // 编译端 wdp 版本
	Values     map[string]any    `json:"values"`      // 全局合并 values（不含 inventory_override；HostPlan.Values 才是每主机实际生效值）
	Meta       chart.Meta        `json:"meta"`        // chart.yaml 快照（marker 处置/相位属性/敏感键口径）
	Helpers    string            `json:"helpers"`     // CollectHelpers 合并结果（含子 chart，渲染可复现）
	Files      map[string]string `json:"files"`       // chart 树快照：相对路径 → base64 内容（仅小文件，apply 侧物化后按原结构加载）
	Payloads   []PayloadRef      `json:"payloads"`    // 超限大文件引用（不进 plan 本体；apply 时经 --chart-dir 补齐）
	Hosts      []*HostPlan       `json:"hosts"`       // 每 (play, host) 一条：多 play 相位产生多批条目
	// Relays 记录 via 中继根的连接元数据（中继机不一定是部署目标，其连接
	// 信息不在 Hosts 里；自治提交按根分组时需要）。
	Relays map[string]HostConn `json:"relays,omitempty"`
}

// PayloadRef 是不嵌入 plan 本体的大文件引用（路径+尺寸+摘要，确定性）。
// 制品类内容（examples/*/packages）经 artifact 模块按 URL 分发或经
// --chart-dir 本地补齐——plan 是配置与意图的载体，不是制品通道。
type PayloadRef struct {
	Path   string `json:"path"` // 相对 chart 根的路径
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// HostPlan 是单主机在单个 play 中的计划分片。
type HostPlan struct {
	PlayIdx  int             `json:"play_idx"` // 所属 play 序号（apply 侧重建 play 编排与批次）
	Host     string          `json:"host"`
	Conn     HostConn        `json:"conn"`           // 连接元数据（不含明文密钥；env: 引用保留）
	Values   map[string]any  `json:"values"`         // 该主机实际生效 values（已应用 inventory_override）
	Vars     map[string]any  `json:"vars"`           // 冻结变量域：inventory vars + values + play vars + 内置变量快照 + fact cache
	Play     PlayMeta        `json:"play"`           // play 级编排属性（become/serial/strategy/environment）
	Pre      []*ResolvedTask `json:"pre,omitempty"`  // pre_<phase> hook
	Tasks    []*ResolvedTask `json:"tasks"`          // 主任务列表（hook 拆分后）
	Post     []*ResolvedTask `json:"post,omitempty"` // post_<phase> hook
	Handlers []*ResolvedTask `json:"handlers,omitempty"`
}

// PlayMeta 是 play 级编排属性的快照。
type PlayMeta struct {
	Name        string            `json:"name,omitempty"`
	Hosts       string            `json:"hosts"` // 原始 hosts 模式（展示用；apply 侧直接用计划内主机清单）
	Become      bool              `json:"become,omitempty"`
	BecomeUser  string            `json:"become_user,omitempty"`
	Serial      string            `json:"serial,omitempty"`
	Strategy    *model.Strategy   `json:"strategy,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Vars        map[string]any    `json:"vars,omitempty"` // play vars（展示/审查用；apply 侧已烘焙进 Vars）
}

// HostConn 是可安全落盘的连接元数据子集。明文密钥（Password/
// KeyPassphrase/BecomePassword 字面值）不进 plan：仅保留 env 间接引用
// （PasswordEnv / "env:VAR" 前缀），apply 时从环境解析。
type HostConn struct {
	Address            string `json:"address,omitempty"`
	Port               int    `json:"port,omitempty"`
	User               string `json:"user,omitempty"`
	Conn               string `json:"conn,omitempty"`
	AgentURL           string `json:"agent_url,omitempty"`
	AgentPort          int    `json:"agent_port,omitempty"`
	TLS                bool   `json:"tls,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
	TLSSkipHostVerify  bool   `json:"tls_skip_host_verify,omitempty"`
	TLSServerName      string `json:"tls_server_name,omitempty"`
	CAFile             string `json:"ca_file,omitempty"`
	CertFile           string `json:"cert_file,omitempty"`
	KeyFile            string `json:"key_file,omitempty"`
	HostKeyCheck       bool   `json:"host_key_check,omitempty"`
	KnownHosts         string `json:"known_hosts,omitempty"`
	ConnectTimeoutSec  int    `json:"connect_timeout_sec,omitempty"`

	// Via 是该主机的网段中继链（从第一跳到根，docs/15 §7.4；空 = 控制端直连）
	Via []string `json:"via,omitempty"`

	// 密钥仅保留 env 间接引用（env:VAR 前缀的 direct 值转写为 envRef）
	PasswordEnv       string `json:"password_env,omitempty"`
	PasswordEnvRef    string `json:"password_env_ref,omitempty"` // 原值为 "env:VAR" 时保留
	KeyPassphraseEnv  string `json:"key_passphrase_env,omitempty"`
	BecomePasswordEnv string `json:"become_password_env,omitempty"`
	BecomePasswordRef string `json:"become_password_ref,omitempty"` // 原值为 "env:VAR" 时保留
}

// ResolvedTask 是计划中的单个任务：model.Task 的可序列化镜像 + 计划专有
// 字段（主机内稳定序号 idx 是 journal 的键；rollback 是模块声明的能力分级）。
// when/loop/模板保留原样——可能依赖运行时 register，不能在编译期求值。
type ResolvedTask struct {
	Idx      int    `json:"idx"`
	Label    string `json:"label"`
	Module   string `json:"module"`
	Rollback string `json:"rollback"` // full|partial|none|readonly（模块声明）
	Hook     string `json:"hook,omitempty"`

	ChartRef  string         `json:"chart_ref,omitempty"` // 子 chart 引用（执行侧按计划内 chart 树展开）
	TasksFrom string         `json:"tasks_from,omitempty"`
	ChartVars map[string]any `json:"chart_vars,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	FreeForm  string         `json:"free_form,omitempty"`

	When         []string          `json:"when,omitempty"`
	Loop         []any             `json:"loop,omitempty"`
	LoopVar      string            `json:"loop_var,omitempty"`
	Register     string            `json:"register,omitempty"`
	Notify       []string          `json:"notify,omitempty"`
	Tags         []string          `json:"tags,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
	IgnoreErrors bool              `json:"ignore_errors,omitempty"`
	Retries      int               `json:"retries,omitempty"`
	DelaySec     int               `json:"delay_sec,omitempty"`
	TimeoutSec   int               `json:"timeout_sec,omitempty"`
	Become       *bool             `json:"become,omitempty"`
	BecomeUser   string            `json:"become_user,omitempty"`
	ChangedWhen  string            `json:"changed_when,omitempty"`
	FailedWhen   string            `json:"failed_when,omitempty"`
	Until        string            `json:"until,omitempty"`
	Output       string            `json:"output,omitempty"`
	NoLog        bool              `json:"no_log,omitempty"`
	DelegateTo   string            `json:"delegate_to,omitempty"`
	RunOnce      bool              `json:"run_once,omitempty"`

	Block  []*ResolvedTask `json:"block,omitempty"`
	Rescue []*ResolvedTask `json:"rescue,omitempty"`
	Always []*ResolvedTask `json:"always,omitempty"`
}

// Shard 返回只含指定主机分片的新计划（自治提交的最小单位：每台 agent
// 只收到并执行自己负责的分片；分片内容寻址——同一分片重复提交幂等，
// journal 续跑的分片键稳定）。
func (p *Plan) Shard(hosts []string) *Plan {
	keep := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		keep[h] = true
	}
	sh := *p
	sh.Hosts = nil
	for _, hp := range p.Hosts {
		if keep[hp.Host] {
			sh.Hosts = append(sh.Hosts, hp)
		}
	}
	sh.Relays = nil // 分片自治执行不再继续下发
	sh.FillID()
	return &sh
}

// Host 返回计划内的主机名列表（按出现序去重）。
func (p *Plan) Host() []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range p.Hosts {
		if !seen[h.Host] {
			seen[h.Host] = true
			out = append(out, h.Host)
		}
	}
	return out
}

// HostPlansOf 返回指定主机的全部计划分片。
func (p *Plan) HostPlansOf(host string) []*HostPlan {
	var out []*HostPlan
	for _, h := range p.Hosts {
		if h.Host == host {
			out = append(out, h)
		}
	}
	return out
}

// ComputeID 计算 plan 的内容寻址 ID：sha256 over canonical JSON。
// PlanID 自身与全部时间性字段不参与（当前结构无时间戳字段）。
func (p *Plan) ComputeID() string {
	c := *p
	c.PlanID = ""
	b, err := json.Marshal(&c)
	if err != nil {
		// map[string]any 含不可序列化值只可能来自编码缺陷，编译期早已拦截
		return "unserializable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FillID 填充 PlanID（幂等：内容不变则 ID 不变）。
func (p *Plan) FillID() { p.PlanID = p.ComputeID() }

// Write 落盘 plan.json（0600：values 可能含敏感配置）。
func (p *Plan) Write(path string) error {
	p.FillID()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Load 读取 plan.json 并校验结构版本与 PlanID 完整性。
func Load(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("failed to parse plan %s: %w", path, err)
	}
	if p.SchemaVer != SchemaVer {
		return nil, fmt.Errorf("plan %s has schema %d, this wdp supports %d (recompile the plan)", path, p.SchemaVer, SchemaVer)
	}
	if id := p.ComputeID(); id != p.PlanID {
		return nil, fmt.Errorf("plan %s content hash mismatch: file says %s, computed %s (file corrupted or edited)", path, p.PlanID, id)
	}
	return &p, nil
}

// Materialize 把 plan 内嵌的 chart 树写出到 dir（0700 目录 / 0600 文件），
// 返回 chart 根目录路径——apply 侧在此之上走 chart.Load 的常规加载路径，
// 保证与编译端的展开/渲染/schema 语义完全一致（单一实现，零漂移）。
func (p *Plan) Materialize(dir string) (string, error) {
	root := filepath.Join(dir, "chart")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	var rels []string
	for rel := range p.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if err := safeWrite(root, rel, p.Files[rel]); err != nil {
			return "", err
		}
	}
	return root, nil
}

// safeWrite 在 root 下按相对路径写出 base64 内容（拒绝越界路径）。
func safeWrite(root, rel, b64 string) error {
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return fmt.Errorf("plan file %q escapes the chart root", rel)
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("plan file %q: invalid base64: %v", rel, err)
	}
	dst := filepath.Join(root, clean)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
