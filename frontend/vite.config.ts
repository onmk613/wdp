import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发模式：vite dev server (5173) 代理后端 API 到 wdp server (7603)，
// 热更新无需重新 go build。生产：vite build → dist/，由 build.sh 拷入
// internal/web/static/ 经 go:embed 打进二进制。
export default defineConfig({
  plugins: [vue()],
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
  },
})
