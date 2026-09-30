//go:build unix

package fsatomic

import "os"

// syncDir 落盘目录项（rename 的持久化）：目录以只读打开，Sync 只刷
// 元数据，开销远小于文件级 fsync。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func(d *os.File) {
		_ = d.Close()
	}(d)
	return d.Sync()
}
