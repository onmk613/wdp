// Package module 实现内置模块。模块只依赖 connection 原语，
// 在控制端编排（exec/upload/download），因此 SSH 与 agent 行为完全一致。
package module

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"wdp/internal/connection"
	"wdp/internal/model"
	"wdp/internal/render"
	"wdp/internal/shellquote"
)

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
	Rollback   *RollbackCtx   // auto_rollback 时的变更日志（nil = 不记录）
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

func argBool(args map[string]any, key string) (bool, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return false, false
	}
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		b, err := strconv.ParseBool(x)
		return b, err == nil
	default:
		return false, false
	}
}

// argMode 解析 mode 参数："0755" / 0755(yaml 八进制整数) / 493。
func argMode(args map[string]any, key string) (fs.FileMode, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return fs.FileMode(x), true
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
