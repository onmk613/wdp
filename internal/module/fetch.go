package module

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wdp/internal/i18n"
)

func init() {
	Register(&FetchModule{})
}

// FetchModule 将远端文件拉取回控制机。
type FetchModule struct{}

// Name 模块名。
func (m *FetchModule) Name() string { return "fetch" }

// Desc 模块说明。
func (m *FetchModule) Desc() string {
	return i18n.T("fetch remote files to the local side", "拉取远端文件到本地")
}

// Run 拉取远端文件到本地（sha256 幂等：本地已是同内容则跳过下载）。
func (m *FetchModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	src, ok := argStr(args, "src")
	if !ok || src == "" {
		return Fail("%s", i18n.T("fetch requires a src parameter", "fetch 需要 src 参数"))
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("%s", i18n.T("fetch requires a dest parameter", "fetch 需要 dest 参数"))
	}
	flat, _ := argBool(args, "flat")
	if _, err := os.Stat(dest); err == nil && !isDirLocal(dest) {
		return Fail(i18n.T("fetch dest must be a directory: %s", "fetch 的 dest 应为目录: %s"), dest)
	}

	remote, exists, bad := remoteChecksum(rc, src)
	if bad != nil {
		return bad
	}
	if !exists {
		return Fail(i18n.T("remote file does not exist: %s", "远端文件不存在: %s"), src)
	}

	local := m.localPath(rc, src, dest, flat)
	if err := checkLocalPath(dest, local); err != nil {
		return Fail("%v", err)
	}
	// 幂等：本地已存在同校验和文件则跳过下载（只读模块，check 模式行为不变）
	if data, err := os.ReadFile(local); err == nil {
		if sum := sha256hex(data); sum == remote {
			return &Result{Msg: fmt.Sprintf(i18n.T("%s is already up to date (sha256 matches)", "%s 已是最新（sha256 一致）"), local)}
		}
	}

	if rc.CheckMode {
		return &Result{Changed: true, Msg: fmt.Sprintf(i18n.T("[check] will fetch %s to %s", "[check] 将拉取 %s 到 %s"), src, local)}
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return Fail(i18n.T("failed to create local directory: %v", "创建本地目录失败: %v"), err)
	}
	// 先下载到临时文件、成功后原子改名：直接截断打开本地文件会在远端
	// 读取失败时把已有旧文件清零，不可恢复
	tmp, err := os.CreateTemp(filepath.Dir(local), ".wdp-fetch-*")
	if err != nil {
		return Fail(i18n.T("failed to create local file: %v", "创建本地文件失败: %v"), err)
	}
	tmpName := tmp.Name()
	if err := rc.Conn.DownloadFile(rc.Ctx, src, tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Fail(i18n.T("download failed: %v", "下载失败: %v"), err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Fail(i18n.T("failed to close local file: %v", "关闭本地文件失败: %v"), err)
	}
	_ = os.Chmod(tmpName, 0o644)
	if err := os.Rename(tmpName, local); err != nil {
		_ = os.Remove(tmpName)
		return Fail(i18n.T("failed to move the fetched file into place: %v", "落盘改名失败: %v"), err)
	}
	return &Result{Changed: true, Msg: fmt.Sprintf(i18n.T("fetched %s to %s", "已拉取 %s 到 %s"), src, local)}
}

// localPath 计算本地落盘路径：
// flat=false 层级模式 dest/<主机名>/<原路径去首斜杠>；flat=true 拍平为 dest/<文件名>。
func (m *FetchModule) localPath(rc *RunContext, src, dest string, flat bool) string {
	if flat {
		return filepath.Join(dest, filepath.Base(src))
	}
	rel := strings.TrimPrefix(src, "/")
	return filepath.Join(dest, rc.Host.Name, filepath.FromSlash(rel))
}

// checkLocalPath 校验最终落盘路径仍位于 dest 之内（防 src/主机名中的 .. 逃逸）。
// 复用 chart.go 已验证的 Clean-后判包含模式；dest 相对路径时基于当前目录解析。
func checkLocalPath(dest, local string) error {
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf(i18n.T("failed to resolve dest: %w", "解析 dest 失败: %w"), err)
	}
	absLocal, err := filepath.Abs(local)
	if err != nil {
		return fmt.Errorf(i18n.T("failed to resolve target path: %w", "解析目标路径失败: %w"), err)
	}
	absDest = filepath.Clean(absDest)
	absLocal = filepath.Clean(absLocal)
	if absLocal != absDest && !strings.HasPrefix(absLocal, absDest+string(os.PathSeparator)) {
		return fmt.Errorf("fetch 落盘路径 %q 逃逸出 dest %q（src 或主机名包含 .. 时触发）", local, dest)
	}
	return nil
}

func isDirLocal(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Params 参数文档。
func (m *FetchModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "远端源文件路径（必需）"},
		{Name: "dest", Type: "string", Desc: "本地目标目录（必需）"},
		{Name: "flat", Type: "bool", Default: "false", Desc: "false 存 dest/<主机>/<路径>；true 拍平到 dest/文件名"},
	}
}

// Example 示例任务。
func (m *FetchModule) Example() string {
	return `- name: 收集各主机日志
  fetch:
    src: /var/log/app/error.log
    dest: ./logs
`
}
