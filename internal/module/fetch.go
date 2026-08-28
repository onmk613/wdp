package module

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	return "fetch remote files to the local side"
}

// Run 拉取远端文件到本地（sha256 幂等：本地已是同内容则跳过下载）。
func (m *FetchModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	src, ok := argStr(args, "src")
	if !ok || src == "" {
		return Fail("%s", "fetch requires a src parameter")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("%s", "fetch requires a dest parameter")
	}
	flat, _ := argBool(args, "flat")
	if _, err := os.Stat(dest); err == nil && !isDirLocal(dest) {
		return Fail("fetch dest must be a directory: %s", dest)
	}

	remote, exists, bad := remoteChecksum(rc, src)
	if bad != nil {
		return bad
	}
	if !exists {
		return Fail("remote file does not exist: %s", src)
	}

	local := m.localPath(rc, src, dest, flat)
	if err := checkLocalPath(dest, local); err != nil {
		return Fail("%v", err)
	}
	// 幂等：本地已存在同校验和文件则跳过下载（只读模块，check 模式行为不变）
	if data, err := os.ReadFile(local); err == nil {
		if sum := sha256hex(data); sum == remote {
			return &Result{Msg: fmt.Sprintf("%s is already up to date (sha256 matches)", local)}
		}
	}

	if rc.CheckMode {
		return &Result{Changed: true, Msg: fmt.Sprintf("[check] will fetch %s to %s", src, local)}
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return Fail("failed to create local directory: %v", err)
	}
	// 先下载到临时文件、成功后原子改名：直接截断打开本地文件会在远端
	// 读取失败时把已有旧文件清零，不可恢复
	tmp, err := os.CreateTemp(filepath.Dir(local), ".wdp-fetch-*")
	if err != nil {
		return Fail("failed to create local file: %v", err)
	}
	tmpName := tmp.Name()
	if err := rc.Conn.DownloadFile(rc.Ctx, src, tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Fail("download failed: %v", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Fail("failed to close local file: %v", err)
	}
	_ = os.Chmod(tmpName, 0o644)
	if err := os.Rename(tmpName, local); err != nil {
		_ = os.Remove(tmpName)
		return Fail("failed to move the fetched file into place: %v", err)
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("fetched %s to %s", src, local)}
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
		return fmt.Errorf("failed to resolve dest: %w", err)
	}
	absLocal, err := filepath.Abs(local)
	if err != nil {
		return fmt.Errorf("failed to resolve target path: %w", err)
	}
	absDest = filepath.Clean(absDest)
	absLocal = filepath.Clean(absLocal)
	if absLocal != absDest && !strings.HasPrefix(absLocal, absDest+string(os.PathSeparator)) {
		return fmt.Errorf("fetch destination path %q escapes dest %q (triggered when src or hostname contains ..)", local, dest)
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
		{Name: "src", Type: "string", Desc: "remote source file path (required)"},
		{Name: "dest", Type: "string", Desc: "local destination directory (required)"},
		{Name: "flat", Type: "bool", Default: "false", Desc: "false stores under dest/<host>/<path>; true flattens to dest/<filename>"},
	}
}

// Example 示例任务。
func (m *FetchModule) Example() string {
	return `- name: collect logs from each host
  fetch:
    src: /var/log/app/error.log
    dest: ./logs
`
}
