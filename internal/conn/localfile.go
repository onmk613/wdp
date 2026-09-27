package conn

// local 与 selfexec 通道共用的本机文件传输实现：两包是兄弟包（均依赖
// 本包），原先 UploadFile/DownloadFile 近乎逐行重复，共享实现放本包
// 导出、各通道薄封装到 Conn 接口方法。

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"wdp/internal/fsatomic"
)

// WriteLocalFile 把 r 的内容写入本机 dst：先建父目录（0755），再经
// fsatomic 原子落盘（同目录临时文件 + fsync + rename——io.Copy 只进页
// 缓存，断电后 rename 落地的可能是空文件/半截文件，copy 模块的
// "已传输"承诺会被系统性违背）。mode 0 折叠为 0644（与原 local/
// selfexec 行为一致，避免 rename 出不可读文件）。
func WriteLocalFile(dst string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	return fsatomic.WriteFile(dst, r, mode)
}

// ReadLocalFile 把本机 src 的内容流式拷贝到 w（不经内存全量缓冲）。
func ReadLocalFile(src string, w io.Writer) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
