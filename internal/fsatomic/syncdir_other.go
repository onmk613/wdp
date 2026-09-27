//go:build !unix

package fsatomic

// syncDir 在不支持目录 fsync 的平台（windows 等）是 no-op：操作系统
// 没有等价 API，目录项持久化交由文件系统自身的一致性机制兜底。
func syncDir(string) error { return nil }
