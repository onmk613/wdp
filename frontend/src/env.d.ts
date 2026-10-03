/// <reference types="vite/client" />

declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<{}, {}, any>
  export default component
}

declare module 'element-plus/dist/locale/zh-cn.mjs'

// 命令式 API（message/message-box）与 v-loading 指令的样式入口（main.ts
// 显式导入）：element-plus 未给这些 style 入口配 .d.ts，这里声明为空模块
// （按需插件在模板组件上生成的同名导入也一并覆盖）
declare module 'element-plus/es/components/*/style/css'

// 构建标识（vite define 注入，见 vite.config.ts）
declare const __APP_BUILD_ID__: string

