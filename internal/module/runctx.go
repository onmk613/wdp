package module

// 模块执行上下文：单主机、单任务项的运行环境与连接执行辅助。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"time"

	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/render"
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
	Conn       conn.Conn
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
	// MaxDownloadBytes 是 get_url 下载响应体上限（字节；0 = 内置默认 2GiB）。
	// 由组合根经 CLI --max-download-mb / wdp.cfg [transfer].max_download_mb 注入。
	MaxDownloadBytes int64
	// CheckScriptAllowed 由 executor 依据 chart.yaml 的 check_mode: supported
	// 声明注入：未声明时脚本模块在 check 模式下直接跳过（脚本是外部代码，
	// 无法保证预演安全），避免 --check 意外执行第三方脚本造成变更。
	CheckScriptAllowed bool
	Rollback           *RollbackCtx // auto_rollback 时的变更日志（nil = 不记录）
}

// exec 通过连接执行脚本，自动带上环境变量与提权配置。
func (rc *RunContext) exec(script string) (conn.ExecResult, *Result) {
	return rc.execWithEnv(script, nil)
}

// execWithEnv 在 exec 基础上追加/覆盖环境变量（脚本模块注入 WDP_* 变量用）。
func (rc *RunContext) execWithEnv(script string, extra map[string]string) (conn.ExecResult, *Result) {
	req := conn.ExecRequest{Script: script, TimeoutMs: rc.TimeoutMs}
	if len(rc.Env)+len(extra) > 0 {
		env := map[string]string{}
		maps.Copy(env, rc.Env)
		maps.Copy(env, extra)
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
		return out, &Result{Failed: true, Msg: fmt.Sprintf("execution failed: %v", err)}
	}
	return out, nil
}
