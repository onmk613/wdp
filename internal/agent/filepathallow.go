package agent

// /file 与 /archive 的领地白名单（opt-in）。
//
// 威胁模型：这两个端点对持有客户端证书的一方而言是"任意路径读写"原语
//（PUT /file 任意写 + GET /file 任意读，/archive 任意读 + 任意写）——
// 与 /shutdown 的 files 参数同级（后者已收紧到 agent 领地，见 clean.go
// extraPathAllowed），前者因部署语义需要写任意路径而保持缺省放开。
// --allow-file-path 提供等价的 opt-in 收紧：启用后上传/下载/解压的路径
// 必须落在列出的目录树内。
//
// 判定是词法口径（Abs + Clean + 前缀），与 extraPathAllowed 一致：不解析
// 符号链接——领地内的符号链接仍可指向领地外（若领地内目录树可被其他
// 用户写，须自行保证其不可写，或用 agent 解压侧的链接防护兜底）。
// /exec 不在收紧范围：部署工具的执行语义本身就是全权（checkAuthSafety
// 已保证对外监听必须 mTLS），本开关只收窄文件面。

import (
	"errors"
	"path/filepath"
	"strings"
)

// SetFilePathRoots 设置 /file、/archive 的领地根目录（绝对路径，可多个）。
// 空切片 = 不启用（保持既有"任意路径"行为）；相对路径与文件系统根被
// 拒绝——相对路径随 agent 工作目录漂移，根目录等于没设。启动期一次性
// 设置（ListenAndServe 之前），handler 侧只读。
func (s *Server) SetFilePathRoots(roots []string) error {
	cleaned := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			return errors.New("file path root must be absolute: " + r)
		}
		c := filepath.Clean(r)
		if c == string(filepath.Separator) {
			return errors.New("file path root must not be the filesystem root")
		}
		cleaned = append(cleaned, c)
	}
	s.fileRoots = cleaned
	return nil
}

// filePathAllowed 判定路径是否落在领地内；未启用（无根目录）恒允许，
// 保持既有行为。
func (s *Server) filePathAllowed(p string) bool {
	if len(s.fileRoots) == 0 {
		return true
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)
	for _, root := range s.fileRoots {
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
