package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// staticFS 内嵌前端资源。前端源码在仓库根 frontend/（Vite + Vue 3 +
// Element Plus），构建产物由 build.sh --frontend 拷入 static/——该目录是
// gitignore 的生成物，仅提交 .keep 占位：免 Node 的裸 go build 依然可
// 编译，运行时 / 返回引导页（见 fallbackIndex）。
//
//go:embed all:static
var staticFS embed.FS

// assetsFS static 子树（index.html + assets/*）。
var assetsFS, _ = fs.Sub(staticFS, "static")

// fallbackIndex 前端未构建时的引导页（503）。
const fallbackIndex = `<!DOCTYPE html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>wdp console</title>
<style>body{font:14px/1.8 -apple-system,"PingFang SC",sans-serif;background:#f5f7fa;color:#0f172a}
main{max-width:560px;margin:12vh auto;background:#fff;border:1px solid #e2e8f0;border-radius:10px;padding:32px}
code{background:#f1f5f9;border-radius:4px;padding:2px 6px}</style></head>
<body><main>
<h1 style="margin-top:0">wdp console</h1>
<p>前端资源未构建：本二进制由不带 <code>--frontend</code> 的构建产出。</p>
<p>构建完整控制台：</p>
<pre><code>cd frontend
npm install
npm run build
cd ..
./build.sh --frontend   # 或手动 cp -R frontend/dist/. internal/web/static/ 后重新 go build</code></pre>
<p>API（/api/*）不受影响。</p>
</main></body></html>
`

// indexTag 是内嵌 index.html 的 ETag（进程内一次计算；升级二进制即换
// 指纹，浏览器 304 → 立即拿到新资源引用）。无缓存头时浏览器会启发式
// 缓存 index.html，导致"界面还是旧版、刷新才生效"类问题。
var (
	indexOnce sync.Once
	indexBody []byte
	indexTag  string
)

func indexAsset() ([]byte, string) {
	indexOnce.Do(func() {
		b, err := fs.ReadFile(assetsFS, "index.html")
		if err != nil {
			return
		}
		indexBody = b
		sum := sha256.Sum256(b)
		indexTag = `"` + hex.EncodeToString(sum[:8]) + `"`
	})
	return indexBody, indexTag
}

// handleIndex 返回控制台单页（前端已构建）或引导页（未构建，503）。
// 前端使用 history 路由（/hosts、/apps/3/edit 等真实路径），刷新或直链
// 时服务端没有对应静态文件，必须回退到 index.html 交给前端路由接管；
// /api/*、/enroll/* 下的未知路径维持 404，避免 API 客户端拿到 HTML。
// 单页必须每次协商（no-cache + ETag）：它引用带哈希的资源文件名，允许
// 被缓存会让升级后的浏览器继续跑旧 bundle。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Path; strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/enroll/") {
		http.NotFound(w, r)
		return
	}
	b, tag := indexAsset()
	if b == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(fallbackIndex))
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if tag != "" {
		w.Header().Set("ETag", tag)
		if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// handleAssets 服务前端构建产物（/assets/*）：文件名带内容哈希，可长期
// 强缓存（内容变更即换名，不会读到旧代码）。
func (s *Server) handleAssets() http.Handler {
	fileServer := http.FileServer(http.FS(assetsFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}
