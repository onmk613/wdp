import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import { gzipSync } from 'node:zlib'
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'

// precompress 为每个文本产物生成 .gz 兄弟文件。
// 服务端（internal/web/static.go）命中 Accept-Encoding: gzip 时直接回预压缩
// 件，运行时零开销；nginx 之类的反代也能用 gzip_static 吃现成文件。
//
// 只做 gzip 不做 brotli：.br 再多省 ~15%，但预压缩件会随 go:embed 进二进制，
// 而这份二进制要下发到每台被纳管主机——体积比几个百分点的传输量更值钱
// （需要 brotli 的部署可在反代侧自行压）。
//
// 必须在 writeBundle（磁盘产物已定稿）读文件压缩，不能在 generateBundle
// 压 chunk.code：vite 对动态 import 的 preload 助手注入/占位符
// （__VITE_PRELOAD__）替换发生在产物落盘前的后处理，generateBundle 阶段
// 拿到的还是带未解析占位符的中间代码——曾因此生成与 .js 内容不一致的
// .gz：浏览器走 gzip 直送拿到坏文件，动态 import 全部 ReferenceError，
// 登录后路由静默卡死在登录页（不带 gzip 的请求反而正常，极难排查）。
function precompress(): Plugin {
  return {
    name: 'wdp-precompress',
    apply: 'build',
    enforce: 'post',
    writeBundle(options) {
      const root = resolve(process.cwd(), options.dir ?? 'dist')
      const walk = (dir: string): string[] =>
        readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
          const p = join(dir, e.name)
          return e.isDirectory() ? walk(p) : [p]
        })
      for (const file of walk(root)) {
        if (!/\.(js|css|html|svg|json|ttf)$/.test(file)) continue
        const data = readFileSync(file)
        if (data.length < 1024) continue // 小文件压缩收益不抵请求开销
        writeFileSync(file + '.gz', gzipSync(data, { level: 9 }))
      }
    },
  }
}

// 开发模式：vite dev server (5173) 代理后端 API 到 wdp server (7603)，
// 热更新无需重新 go build。生产：vite build → dist/，由 build.sh 拷入
// internal/web/static/ 经 go:embed 打进二进制。
export default defineConfig({
  plugins: [vue(), precompress()],
  // 构建标识注入 bundle：每次构建必变 → 产物 hash 与构建一一对应。
  // 静态资源按 hash 名 immutable 缓存一年，若两次构建产物同名而内容有异
  // （预压缩事故即此形态），客户端坏缓存无法自愈；构建 ID 进内容后，
  // 任何构建差异都会换成全新文件名，浏览器必然重新拉取。
  define: {
    __APP_BUILD_ID__: JSON.stringify(`${new Date().toISOString()}`),
  },
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
    // <link rel="modulepreload">——登录页因此也要下 3.7MB。这里只在
    // **HTML 入口上下文**剔除它；动态 import 上下文的依赖表保持完整——
    // __vitePreload 靠它预载 monaco 的 CSS/worker，一并剥掉会让 IDE
    // 裸奔无样式。
    // modulePreload：入口 HTML 只预热首屏真正用到的 chunk（防御性：
    // 正常情况下 monaco 不在入口依赖里——它只被动态 import；一旦 chunk
    // 图变化把它带进来，这里兜底剔除，避免登录页预热 3.7MB）。
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
          // vite 的 preload 助手（__vitePreload）是所有动态 import 的公共
          // 依赖，rollup 会把它塞进任意一个 manual chunk——此前落在 monaco
          // 块，入口因此静态 import 整个 monaco：登录页照样执行期拉取
          // 3.9MB JS + 阻塞渲染的 monaco CSS（modulePreload 过滤只藏得掉
          // 预热提示，藏不掉静态 import 的实际拉取）。助手必须钉进入口块。
          if (id.includes('vite/preload-helper')) return 'index'
          if (id.includes('node_modules/element-plus') || id.includes('node_modules/@element-plus')) return 'element'
          if (id.includes('node_modules/monaco-editor') || id.includes('node_modules/monaco-yaml')) return 'monaco'
        },
      },
    },
  },
})
