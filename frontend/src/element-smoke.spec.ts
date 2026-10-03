// element-plus 按需引入冒烟：构建后肉眼核对成本高，这里兜住三件事——
// 1) 模板里的 el-* 能渲染（Components 插件编译期注入没失效）；
// 2) locale 经 ElConfigProvider 生效（分页「共 x 条」等文案不再走默认英文）；
// 3) 命令式 API / v-loading 的样式入口链仍可解析（main.ts 显式导入的
//    message/message-box/loading 样式；element-plus 升级若重排 style 目录，
//    这里第一时间红，而不是线上消息框裸奔。经 vite 管线加载——vitest 配置
//    已把 element-plus 内联，.css 由 vitest 桩掉）。
import { describe, expect, it } from 'vitest'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'
import 'element-plus/es/components/message/style/css'
import 'element-plus/es/components/message-box/style/css'
import 'element-plus/es/components/loading/style/css'
import { vLoading } from 'element-plus/es/components/loading/index'
import Smoke from './element-smoke.fixture.vue'

describe('element-plus 按需引入冒烟', () => {
  it('模板组件经 Components 插件注册并可渲染（SSR）', async () => {
    const html = await renderToString(createSSRApp(Smoke))
    // el-button / el-pagination / config-provider 三类都注入成功才有的类名
    expect(html).toContain('el-button')
    expect(html).toContain('el-pagination')
  })

  it('locale 经 ElConfigProvider 生效（zh-cn 分页总数文案）', async () => {
    const html = await renderToString(createSSRApp(Smoke))
    expect(html).toContain('共 42 条')
  })

  it('v-loading 指令模块可解析且为对象指令（含 mounted 生命周期）', () => {
    expect(vLoading).toBeTruthy()
    expect(typeof (vLoading as { mounted?: unknown }).mounted).toBe('function')
  })
})
