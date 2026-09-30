package fsatomic

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile 把 r 的内容原子写入 path：读者要么看到完整的旧文件，要么
// 看到完整的新文件，不会观察到截断/半写状态（O_TRUNC 原地重写在进程
// 崩溃或磁盘满时会留下永久损坏的半截文件）。
//
// 统一采取的最严格口径（各原实现细节差异的并集上界）：
//   - 临时文件建在 path 所在目录（同文件系统是 rename 原子性的前提），
//     任何失败分支都清理临时文件；
//   - 数据写入后先 chmod 再 fsync（权限作为元数据随 fsync 一并持久；
//     旧实现两种顺序都有，此序更严格），close 后 rename 覆盖目标；
//   - rename 后 fsync 所在目录（持久化 rename 本身：不刷目录，断电后
//     新文件可能整体消失退回旧文件——原五处实现均缺这一步）。目录同步
//     在不支持的平台上是 no-op，见 syncdir_other.go。
//
// mode 原样生效（含 0）；默认权限（如 0 折叠为 0644）由调用方决定。
// 不负责创建父目录（目录缺失时直接失败），需要建目录的调用方先 MkdirAll。
func WriteFile(path string, r io.Reader, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wdp-fsatomic-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 成功路径 rename 之后 tmpName 已不存在，Remove 报 IsNotExist，忽略
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return syncDir(dir)
}
