// Package selfexec 提供 agent 自治执行的本地连接：在 agent 进程所在的
// 主机上执行，语义与远程 agent 通道完全一致——完整实现 become
// （sudo -n 免密 / sudo -S 密码经 stdin），复用 agent.RunScript 的单一实现。
//
// 与 conn/local 的区别（docs/15 §7.2，这是本包存在的全部理由）：
// local 面向控制机演练，显式忽略 become 并告警；selfexec 面向 agent 自治
// 执行，become 任务静默以 agent 自身用户执行比失败更危险，必须真正提权。
package selfexec

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/selfrun"
)

func init() {
	// agent 自治执行把本机主机条目的 conn 改写为 selfexec；become 密码走
	// host.BecomePassword / BecomePasswordEnv 既有机制（"env:VAR" 支持）。
	conn.RegisterFactory("selfexec", func(h *model.Host, _ *conn.Defaults) (conn.Conn, error) {
		return &SelfExec{host: h.Name, becomePassword: model.Secret(h.BecomePassword, h.BecomePasswordEnv)}, nil
	})
}

// SelfExec 是 agent 本机执行连接。BecomePassword 随 plan 一次性下发、
// 仅驻留内存（不落盘到 journal / plan 文件）；免密 sudo 环境留空即可
// （自治执行推荐免密 sudo，带密码是降级路径）。
type SelfExec struct {
	host           string
	becomePassword string
}

// New 构造本机执行连接（host 为主机名，仅用于 Hostname 报告）。
func New(host string) *SelfExec { return &SelfExec{host: host} }

// SetBecomePassword 设置 become 密码（内存态；plan 提交时下发）。
func (s *SelfExec) SetBecomePassword(pw string) { s.becomePassword = pw }

// Connect 本机执行无需建连。
func (s *SelfExec) Connect(context.Context) error { return nil }

// Close 无资源可释放。
func (s *SelfExec) Close() error { return nil }

// Hostname 返回主机名。
func (s *SelfExec) Hostname() string { return s.host }

// Exec 在本机执行脚本（完整 become 语义，与 agent /exec 端点同实现）。
func (s *SelfExec) Exec(ctx context.Context, req conn.ExecRequest) (conn.ExecResult, error) {
	resp := selfrun.RunScript(ctx, selfrun.ExecReq{
		Script:         req.Script,
		Stdin:          req.Stdin,
		Env:            req.Env,
		TimeoutMs:      req.TimeoutMs,
		BecomeUser:     req.BecomeUser,
		BecomePassword: s.becomePassword,
	})
	return conn.ExecResult{Code: resp.Code, Stdout: resp.Stdout, Stderr: resp.Stderr}, nil
}

// UploadFile 写本机文件（临时文件 + 原子改名；与 local 同实现）。
func (s *SelfExec) UploadFile(_ context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".wdp-upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("failed to write %s to disk: %w", dst, err)
	}
	return nil
}

// DownloadFile 读本机文件。
func (s *SelfExec) DownloadFile(_ context.Context, src string, w io.Writer) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
