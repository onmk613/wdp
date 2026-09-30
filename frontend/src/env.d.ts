/// <reference types="vite/client" />

declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<{}, {}, any>
  export default component
}

declare module 'element-plus/dist/locale/zh-cn.mjs'

// 构建标识（vite define 注入，见 vite.config.ts）
declare const __APP_BUILD_ID__: string

