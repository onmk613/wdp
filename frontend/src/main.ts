import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/dist/locale/zh-cn.mjs'
import 'element-plus/dist/index.css'
import App from './App.vue'
import { router } from './router'

// 构建标识（vite define 注入，见 vite.config.ts）：仅用于进 bundle 内容
// 驱动 hash 变化 + 排查时确认前端版本
console.debug(`[wdp console] build ${__APP_BUILD_ID__}`)

createApp(App).use(router).use(ElementPlus, { locale: zhCn }).mount('#app')
