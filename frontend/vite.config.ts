import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import { gzipSync } from 'node:zlib'

// precompress 为每个文本产物生成 .gz 兄弟文件。
// 服务端（internal/web/static.go）命中 Accept-Encoding: gzip 时直接回预压缩
// 件，运行时零开销；nginx 之类的反代也能用 gzip_static 吃现成文件。
//
// 只做 gzip 不做 brotli：.br 再多省 ~15%，但预压缩件会随 go:embed 进二进制，
// 而这份二进制要下发到每台被纳管主机——体积比几个百分点的传输量更值钱
// （需要 brotli 的部署可在反代侧自行压）。
function precompress(): Plugin {
  return {
    name: 'wdp-precompress',
    apply: 'build',
    enforce: 'post',
    generateBundle(_options, bundle) {
      for (const [name, chunk] of Object.entries(bundle)) {
        if (!/\.(js|css|html|svg|json|ttf)$/.test(name)) continue
        const data =
          chunk.type === 'asset'
            ? typeof chunk.source === 'string'
              ? Buffer.from(chunk.source)
              : Buffer.from(chunk.source)
            : Buffer.from(chunk.code)
        if (data.length < 1024) continue // 小文件压缩收益不抵请求开销
        this.emitFile({ type: 'asset', fileName: name + '.gz', source: gzipSync(data, { level: 9 }) })
      }
    },
  }
}

// 开发模式：vite dev server (5173) 代理后端 API 到 wdp server (7603)，
// 热更新无需重新 go build。生产：vite build → dist/，由 build.sh 拷入
// internal/web/static/ 经 go:embed 打进二进制。
export default defineConfig({
  plugins: [vue(), precompress()],
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:7603',
      '/enroll': 'http://127.0.0.1:7603',
    },
  },
  build: {
    outDir: 'dist',
    // 单页控制台，产物即 index.html + assets/；不需要兼容老旧浏览器
    target: 'es2022',
    // modulePreload：只预热首屏真正用到的 chunk。monaco（3.7MB）虽然已经
    // 是动态 import，但默认依赖解析会把它的 chunk 写进 index.html 的
    // <link rel="modulepreload">——登录页因此也要下 3.7MB。这里把它剔除，
    // 等 Chart IDE 真正导航到时再取。
    modulePreload: {
      resolveDependencies: (_filename: string, deps: string[]) =>
        deps.filter((d) => !d.includes('monaco')),
    },
    // 构建期预压缩：产物同时输出 .gz，服务端按 Accept-Encoding 直接送
    // （见 internal/web/static.go）。控制台首屏 ~1.4MB JS，gzip 后 ~450KB。
    // 用 Node 内置 zlib，不引额外依赖。
    assetsInlineLimit: 4096,
    // 拆分大依赖：element-plus 整包与 monaco 各自成块——按需引入改造前，
    // 至少让两者与业务代码分离（独立缓存、并行加载，业务迭代不失效
    // 大体积 vendor 块的浏览器缓存）
    rollupOptions: {
      output: {
        manualChunks(id: string) {
          if (id.includes('node_modules/element-plus') || id.includes('node_modules/@element-plus')) return 'element'
          if (id.includes('node_modules/monaco-editor') || id.includes('node_modules/monaco-yaml')) return 'monaco'
        },
      },
    },
  },
})
