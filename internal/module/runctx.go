package module

// 模块执行上下文：单主机、单任务项的运行环境与连接执行辅助。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"time"

	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/render"
)

// defaultTransferLimit 是传输类上限的内置兜底（2GiB）：本地分发源读取
// （max_upload_mb）、下载响应体（max_download_mb）与归档成员解压总量在
// 配置未注入（<=0）时共用同一缺省——无上限时大文件/解压炸弹可在超时
// 窗口内累积数 GB 内存打爆控制端。
const defaultTransferLimit int64 = 2 << 30

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

// readLocalSrc 读取本地分发源文件并施加 max_upload_mb 上限（字节读取
// 中截断，不依赖 stat——TOCTOU 之外 stat 也不覆盖管道等非常规文件）。
// 超限 fail-loud：分发路径整体驻留内存，forks 并发下误配大文件是控制端
// OOM 的最短路径。
func readLocalSrc(rc *RunContext, path string) ([]byte, error) {
	return readLocalCap(rc, path, rc.MaxUploadBytes)
}

// uploadLimit 归一 max_upload_mb（<=0 = 内置 2GiB）。
func uploadLimit(rc *RunContext) int64 {
	if rc.MaxUploadBytes > 0 {
		return rc.MaxUploadBytes
	}
	return defaultTransferLimit
}

// readLocalCap 读取本地文件并封顶 cap 字节（<=0 = 内置 2GiB）。
// 读中截断 + 超限 fail-loud，防大文件 OOM 控制端。
func readLocalCap(rc *RunContext, path string, cap int64) ([]byte, error) {
	if cap <= 0 {
		cap = defaultTransferLimit
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > cap {
		return nil, fmt.Errorf("%s exceeds the %d MiB limit ([transfer].max_upload_mb)", path, cap>>20)
	}
	return data, nil
}

// cappedBuffer 是带上限的字节缓冲：超过 max 后继续接收但丢弃（不中断
// 上游读取，便于把"超限"作为结果上报而不是当成传输失败），并记录总长度。
// 字段名不用 cap：遮蔽内建 cap() 易在阅读时造成误导。
type cappedBuffer struct {
	buf   bytes.Buffer
	max   int64
	total int64
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	// 只保留前 max 字节，超出部分仅计数（截断与否由 total > max 判定）
	if room := w.max - int64(w.buf.Len()); room > 0 {
		if int64(len(p)) <= room {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:room])
		}
	}
	return len(p), nil
}

// truncated 报告是否发生截断。
func (w *cappedBuffer) truncated() bool { return w.total > w.max }

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
	// MaxDownloadBytes get_url 下载响应体上限（字节；0 = 默认 2GiB），
	// 经 --max-download-mb / wdp.cfg [transfer].max_download_mb 注入。
	MaxDownloadBytes int64
	// MaxUploadBytes copy/unarchive 本地分发源读取上限（字节；0 = 默认 2GiB），
	// 经 wdp.cfg [transfer].max_upload_mb 注入，防 forks 并发下内存打爆。
	MaxUploadBytes int64
	// CheckScriptAllowed 由 executor 依据 chart.yaml 的 check_mode: supported
	// 声明注入：未声明时脚本模块在 check 模式下直接跳过（脚本是外部代码，
	// 无法保证预演安全），避免 --check 意外执行第三方脚本造成变更。
	CheckScriptAllowed bool
	Rollback           *RollbackCtx // auto_rollback 时的变更日志（nil = 不记录）
}

// engine 返回渲染引擎：Engine 为 nil 时回退默认引擎（template/systemd_unit/
// debug 三处共用口径）。
func (rc *RunContext) engine() *render.Engine {
	if rc.Engine == nil {
		return render.DefaultEngine()
	}
	return rc.Engine
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
