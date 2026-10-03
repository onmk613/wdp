import { createApp } from 'vue'
// element-plus 按需引入（插件见 vite.config.ts）：模板里的 el-* 组件在
// 编译期由 Components 插件就地 import，无需 app.use 全量注册。这里只补
// 编译期链路覆盖不到的两样：
// 1) 命令式 API（ElMessage/ElMessageBox）的样式——代码里是显式 import
//    （tree-shaking 生效），但它们没有模板标签，样式不随组件链路进
//    bundle，必须显式导一份（style/css 入口自带 base 等依赖链）；
// 2) v-loading 指令——指令没有组件形态，按需插件只认标签，手动注册。
import { vLoading } from 'element-plus/es/components/loading/index'
import 'element-plus/es/components/loading/style/css'
import 'element-plus/es/components/message/style/css'
import 'element-plus/es/components/message-box/style/css'
import App from './App.vue'
import { router } from './router'

// 构建标识（vite define 注入，见 vite.config.ts）：仅用于进 bundle 内容
// 驱动 hash 变化 + 排查时确认前端版本
console.debug(`[wdp console] build ${__APP_BUILD_ID__}`)

const app = createApp(App)
app.directive('loading', vLoading)
app.use(router).mount('#app')
