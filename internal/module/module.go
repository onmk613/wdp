// Package module 实现内置模块。模块只依赖 connection 原语，
// 在控制端编排（exec/upload/download），因此 SSH 与 agent 行为完全一致。
package module

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"wdp/internal/connection"
	"wdp/internal/model"
	"wdp/internal/render"
	"wdp/internal/shellquote"
)

// tempSuffix 生成不可预测的临时文件后缀（crypto/rand）：
// 远端 /tmp 下 UnixNano 时间戳路径可被本机低权限用户预创建符号链接劫持
// （上传/执行/快照重定向到任意路径）。
func tempSuffix() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()) // crypto/rand 故障兜底
	}
	return hex.EncodeToString(b)
}

// RunContext 是模块执行上下文（单主机、单任务项）。
type RunContext struct {
	Ctx        context.Context
	Conn       connection.Connection
	Host       *model.Host
	Vars       map[string]any // 该主机当前变量域（含 item）
	Env        map[string]string
	Become     bool
	BecomeUser string
	BaseDir    string         // playbook/chart 所在目录，用于解析 src 相对路径
	Engine     *render.Engine // 渲染引擎（含 chart helpers），nil 时用默认引擎
	TimeoutMs  int64          // 任务超时毫秒（0 不限），透传给连接层
	CheckMode  bool           // check 模式：模块预演不实际变更
	DiffMode   bool           // diff 模式：check 下产出内容级差异
	// CheckScriptAllowed 由 executor 依据 chart.yaml 的 check_mode: supported
	// 声明注入：未声明时脚本模块在 check 模式下直接跳过（脚本是外部代码，
	// 无法保证预演安全），避免 --check 意外执行第三方脚本造成变更。
	CheckScriptAllowed bool
	Rollback           *RollbackCtx // auto_rollback 时的变更日志（nil = 不记录）
}

// RollbackAction 是一条可回滚变更：
//   - restore：Path 从 Shadow 快照恢复（覆盖前快照）
//   - remove：删除 Path（本次新建的文件/目录）
type RollbackAction struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Shadow string `json:"shadow,omitempty"`
}

// RollbackCtx 由 executor 注入：远端快照根目录 + 变更记录回调。
type RollbackCtx struct {
	Dir    string
	Record func(RollbackAction)
}

// Snapshot 在变更前把已存在的目标快照到 shadow 区并登记 restore 动作。
// 快照失败不阻塞部署（该文件将无法自动回滚）。
func (rb *RollbackCtx) Snapshot(rc *RunContext, dest string) {
	shadow := rb.Dir + dest
	script := fmt.Sprintf("mkdir -p -- %s && cp -a -- %s %s",
		shellquote.Quote(pathDirOf(shadow)), shellquote.Quote(dest), shellquote.Quote(shadow))
	out, bad := rc.exec(script)
	if bad != nil || out.Code != 0 {
		return
	}
	rb.Record(RollbackAction{Kind: "restore", Path: dest, Shadow: shadow})
}

// RecordRemove 登记新建路径的删除动作。
func (rb *RollbackCtx) RecordRemove(path string) {
	rb.Record(RollbackAction{Kind: "remove", Path: path})
}

func pathDirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}

// Result 是模块执行结果。
type Result struct {
	Changed bool
	Failed  bool
	Skipped bool // 模块主动跳过（如 check 模式下未声明的脚本模块）
	Msg     string
	Stdout  string
	Stderr  string
	Rc      int
	Facts   map[string]any // 非空时并入主机变量域（setup 使用）
	Groups  []string       // 动态组（group_by）：executor 聚合进 inventory，下一批次/play 生效
	Diff    string         // --diff 模式：内容级差异（unified diff 或属性前后对照）
}

// Fail 快速构造失败结果。
func Fail(format string, a ...any) *Result {
	return &Result{Failed: true, Msg: fmt.Sprintf(format, a...)}
}

// Module 是模块接口。实现必须无状态（全局单例注册）。
type Module interface {
	Name() string
	Desc() string
	Run(rc *RunContext, args map[string]any, free string) *Result
}

// ParamDoc 描述模块参数（wdp modules 文档与 wdp new --module 片段生成的单一事实来源）。
type ParamDoc struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // string / list / bool / int / mode / map
	Default string `json:"default,omitempty"`
	Desc    string `json:"desc"`
}

// UsageProvider 模块可选实现：参数自描述 + 示例任务 YAML。
// 实现 后自动进入 `wdp modules -v` 文档与 `wdp new --module` 生成链路。
type UsageProvider interface {
	Params() []ParamDoc
	Example() string
}

// Usage 返回模块参数文档（未实现 UsageProvider 时返回 nil）。
func Usage(m Module) []ParamDoc {
	if up, ok := m.(UsageProvider); ok {
		return up.Params()
	}
	return nil
}

// Example 返回模块示例任务（未实现时返回空串）。
func Example(m Module) string {
	if up, ok := m.(UsageProvider); ok {
		return up.Example()
	}
	return ""
}

var registry = map[string]Module{}

// Register 注册模块（各实现文件 init 调用）。
func Register(m Module) {
	registry[m.Name()] = m
}

// Get 按名查找模块。
func Get(name string) (Module, bool) {
	m, ok := registry[name]
	return m, ok
}

// Names 返回全部模块名（有序）。
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ---- 执行辅助 ----

// exec 通过连接执行脚本，自动带上环境变量与提权配置。
func (rc *RunContext) exec(script string) (connection.ExecResult, *Result) {
	return rc.execWithEnv(script, nil)
}

// execWithEnv 在 exec 基础上追加/覆盖环境变量（脚本模块注入 WDP_* 变量用）。
func (rc *RunContext) execWithEnv(script string, extra map[string]string) (connection.ExecResult, *Result) {
	req := connection.ExecRequest{Script: script, TimeoutMs: rc.TimeoutMs}
	if len(rc.Env)+len(extra) > 0 {
		env := map[string]string{}
		for k, v := range rc.Env {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		req.Env = env
	}
	if rc.Become {
		u := rc.BecomeUser
		if u == "" {
			u = "root"
		}
		req.BecomeUser = u
	}
	out, err := rc.Conn.Exec(rc.Ctx, req)
	if err != nil {
		return out, &Result{Failed: true, Msg: fmt.Sprintf("执行失败: %v", err)}
	}
	return out, nil
}

// ---- 参数解析辅助 ----

func argStr(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	default:
		return fmt.Sprint(x), true
	}
}

// argStrList 接受字符串（空白分割）或列表。
func argStrList(args map[string]any, key string) ([]string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false
	}
	switch x := v.(type) {
	case string:
		fields := strings.Fields(x)
		if len(fields) == 0 {
			return nil, true
		}
		return fields, true
	case []any:
		out := make([]string, 0, len(x))
		for _, it := range x {
			out = append(out, fmt.Sprint(it))
		}
		return out, true
	default:
		return []string{fmt.Sprint(x)}, true
	}
}

// argBool 解析布尔参数。除 Go 原生 true/false 外，
// 兼容 YAML 1.1 / Ansible 惯用写法 yes/no/on/off/1/0（大小写不敏感）。
// 无法解析时返回 ok=false（视为未提供）；ValidateArgs 会在派发前对
// bool 类型参数做严格校验，因此这里不会把 "maybe" 静默当 false。
func argBool(args map[string]any, key string) (bool, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return false, false
	}
	switch x := v.(type) {
	case bool:
		return x, true
	case int:
		if x == 0 {
			return false, true
		}
		if x == 1 {
			return true, true
		}
		return false, false
	case float64: // YAML 解析器可能产出浮点
		if x == 0 {
			return false, true
		}
		if x == 1 {
			return true, true
		}
		return false, false
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
	}
	return false, false
}

// argMode 解析 mode 参数："0755" / 0755(yaml 八进制整数) / 493。
// yaml.v3 对含 8/9 的纯数字（如 0999）会解析成 float64——八进制里
// 8/9 非法，一律拒绝（ok=false），不再静默丢弃。
func argMode(args map[string]any, key string) (fs.FileMode, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return fs.FileMode(x), true
	case float64:
		if x != float64(int64(x)) || x < 0 || x >= 512 {
			return 0, false
		}
		return fs.FileMode(int64(x)), true
	case string:
		s := strings.TrimSpace(x)
		base := 10
		if strings.HasPrefix(s, "0") && len(s) > 1 && !strings.ContainsAny(s, "89") {
			base = 8
		}
		n, err := strconv.ParseUint(s, base, 32)
		if err != nil {
			return 0, false
		}
		return fs.FileMode(n), true
	default:
		return 0, false
	}
}

// ValidateArgs 在模块派发前校验任务参数（executor 调用）：
//   - 未知键直接报错（附相似键建议）——杜绝 moed/ownerr 之类拼写错误静默失效；
//   - bool 类型参数拒绝无法解析的值（如 "maybe"）；
//   - mode 类型参数拒绝非法值。
//
// free 非空表示任务使用了 free-form 写法（如 `command: ls -l`），
// 仅声明了 "(free-form)" 参数的模块接受该写法。
// 未实现 UsageProvider（无参数元数据）时跳过校验。
func ValidateArgs(m Module, args map[string]any, free string) error {
	params := Usage(m)
	if params == nil {
		return nil
	}
	allowed := make(map[string]ParamDoc, len(params))
	for _, p := range params {
		allowed[p.Name] = p
	}
	if free != "" {
		if _, ok := allowed["(free-form)"]; !ok {
			return fmt.Errorf("模块 %s 不接受 free-form 写法", m.Name())
		}
	}
	for k, v := range args {
		doc, ok := allowed[k]
		if !ok {
			if s := suggestKey(k, params); s != "" {
				return fmt.Errorf("模块 %s 存在未知参数 %q（是否为 %q？）", m.Name(), k, s)
			}
			return fmt.Errorf("模块 %s 存在未知参数 %q", m.Name(), k)
		}
		switch doc.Type {
		case "bool":
			if _, ok := argBool(args, k); !ok {
				return fmt.Errorf("模块 %s 参数 %s 需要布尔值（true/false/yes/no），得到 %v", m.Name(), k, v)
			}
		case "mode":
			if _, ok := argMode(args, k); !ok {
				return fmt.Errorf("模块 %s 参数 %s 需要合法权限值（如 \"0755\"），得到 %v", m.Name(), k, v)
			}
		}
	}
	return nil
}

// suggestKey 返回与未知键最相似的合法参数名（编辑距离 ≤ 2 时给出建议）。
func suggestKey(unknown string, params []ParamDoc) string {
	best, bestDist := "", 3
	for _, p := range params {
		d := editDistance(unknown, p.Name)
		if d < bestDist {
			best, bestDist = p.Name, d
		}
	}
	return best
}

// editDistance 经典 Levenshtein 距离（参数名都很短，无需优化）。
func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	dp := make([][]int, la+1)
	for i := range dp {
		dp[i] = make([]int, lb+1)
		dp[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		dp[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			dp[i][j] = min(dp[i-1][j]+1, dp[i][j-1]+1, dp[i-1][j-1]+cost)
		}
	}
	return dp[la][lb]
}
