// 暂存-保存状态机（自 AppIDE 抽取）：草稿 dirty 跟踪、自动/手动暂存
// （DraftStore：服务端为主 + localStorage 兜底）、保存确认流程（版本号/
// 描述写回 chart.yaml → 校验门禁 → WARN 二次确认 → 提交入库）、离开
// 路由守卫（暂存/丢弃/留下三选）。草稿恢复的整体重建（fs + models）
// 横跨多个 composable，留在组件侧，经 setSuppressAutosave/stashed 协作。
//
// 校验本体（问题面板、markers）留在组件：这里只依赖 validate() 回调，
// 失败返回 null（区别于「校验通过、无发现」的 []）。

import { onBeforeUnmount, onMounted, ref } from 'vue'
import type { Ref } from 'vue'
import { onBeforeRouteLeave, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import type * as Monaco from 'monaco-editor'
import { api } from '../api'
import {
  chartYAMLVersion, patchChartYAMLDescription, patchChartYAMLVersion, type ChartFS, type DraftPayload,
} from './fs'
import type { DraftStore } from './draft'
import type { ProblemItem } from './validate'

export interface DraftFlowOptions {
  fs: ChartFS
  draftStore: DraftStore
  // 校验进行中（组件侧 doValidate 维护）：保存流程的并发门禁之一
  validating: Ref<boolean>
  // 初始化未完成时不自动暂存
  loading: Ref<boolean>
  isCreate: () => boolean
  appId: () => number
  // 草稿快照携带的 UI 状态
  openTabs: () => string[]
  activeTab: () => string
  // 保存时同步 chart.yaml 的 model（版本号 patch 后回写编辑器）
  models: Map<string, Monaco.editor.ITextModel>
  // 已占用版本号（保存成功后追加）
  existingVersions: Ref<string[]>
  // 校验门禁（组件侧 doValidate）：失败返回 null
  validate: () => Promise<ProblemItem[] | null>
  // 初始化完成标志（离开守卫在初始化前不拦）
  isReady: () => boolean
}

export function useDraftFlow(opts: DraftFlowOptions) {
  const router = useRouter()
  const draftInfo = ref<{ payload: DraftPayload; updated_at: string } | null>(null)
  const draftStatus = ref('')
  const saving = ref(false)
  const saveVisible = ref(false)

  // 暂存安全标记：true = 当前改动已成功暂存（离开不再弹确认框）。
  // 会话失效（401）标记：停止自动暂存——否则每 2.5s 一轮 401，全局事件
  // 反复把路由推向登录页，离开守卫会跟着反复弹框（曾在编辑中被循环打断）。
  // 窗口重新获得焦点时探测一次会话（用户可能在别的标签重新登录了），
  // 恢复则解除标记并续上自动暂存
  const stashed = ref(false)
  const authDead = ref(false)
  const onUnauthorizedEvt = () => { authDead.value = true }
  async function onWinFocus() {
    if (!authDead.value) return
    try {
      await api('GET', '/api/me')
      authDead.value = false
      draftStatus.value = ''
      scheduleAutosave()
    } catch { /* 会话仍失效：维持暂停 */ }
  }
  onMounted(() => {
    window.addEventListener('wdp-unauthorized', onUnauthorizedEvt)
    window.addEventListener('focus', onWinFocus)
  })
  onBeforeUnmount(() => {
    window.removeEventListener('wdp-unauthorized', onUnauthorizedEvt)
    window.removeEventListener('focus', onWinFocus)
  })

  // ---- 暂存 ----
  let autosaveTimer = 0
  let suppressAutosave = false
  // 草稿恢复期间整体重建 fs/models 会触发成片的内容事件，暂停自动暂存
  //（恢复完由调用方恢复，并置 stashed）
  function setSuppressAutosave(v: boolean) { suppressAutosave = v }
  function scheduleAutosave() {
    if (suppressAutosave) return
    clearTimeout(autosaveTimer)
    autosaveTimer = window.setTimeout(autoDraft, 2500)
  }
  async function autoDraft() {
    const { fs, draftStore, loading } = opts
    if (!fs.isDirty() || suppressAutosave || loading.value || authDead.value) return
    try {
      await draftStore.save(fs.toDraftPayload(opts.openTabs(), opts.activeTab()), fs.baseVersion)
      stashed.value = true
      draftStatus.value = `已自动暂存 ${new Date().toLocaleTimeString('zh-CN', { hour12: false })}`
    } catch {
      stashed.value = false
      if (authDead.value) {
        draftStatus.value = '登录已失效：暂存中断（本地仍有快照）'
        return
      }
      // 瞬时网络故障：10s 后重试（此前只显示「重试中」却不排重试，下一次
      // 内容变化前暂存一直停摆）
      draftStatus.value = '自动暂存失败（重试中）'
      clearTimeout(autosaveTimer)
      autosaveTimer = window.setTimeout(autoDraft, 10000)
    }
  }
  async function manualDraft() {
    try {
      await opts.draftStore.save(opts.fs.toDraftPayload(opts.openTabs(), opts.activeTab()), opts.fs.baseVersion)
      stashed.value = true
      draftStatus.value = `已暂存 ${new Date().toLocaleTimeString('zh-CN', { hour12: false })}`
      ElMessage.success('已暂存（跨设备可恢复）')
    } catch (e) {
      ElMessage.error(`暂存失败：${(e as Error).message}`)
    }
  }
  async function discardDraft() {
    await opts.draftStore.clear()
    draftInfo.value = null
  }

  function onBeforeUnload(e: BeforeUnloadEvent) {
    if (opts.fs.isDirty()) {
      e.preventDefault()
      e.returnValue = ''
    }
  }

  // ---- 保存 ----
  function openSave() {
    // 校验阶段（validating）也占着保存流程：不加门禁的话，慢校验期间再点
    // 保存/Ctrl+S 会并发跑两份 onSaveConfirm，第二个 PUT 因版本已存在报
    // "保存失败"误导用户（第一次实际已成功）
    if (opts.validating.value || saving.value) return
    if (!opts.fs.isDirty() && !opts.isCreate()) {
      ElMessage.info('没有改动')
      return
    }
    saveVisible.value = true
  }

  async function onSaveConfirm(v: { version: string; description: string; pools: string[]; groups: string[]; labels: string }) {
    const { fs, draftStore, models, existingVersions } = opts
    if (opts.validating.value || saving.value) return
    // saving 覆盖全程（校验 + 警告确认 + 提交）：期间按钮 loading、Ctrl+S
    // 与重复确认都被挡住，杜绝并发 PUT
    saving.value = true
    try {
      // 1) 版本号/描述同步写进 chart.yaml（文件与库一致）
      const chart = fs.get('chart.yaml')
      if (chart) {
        let content = chart.content
        const desc = v.description.trim()
        if (chartYAMLVersion(content) !== v.version) content = patchChartYAMLVersion(content, v.version)
        // 有 description 行则替换（旧值不残留），没有则追加；空描述不动文件
        if (desc) content = patchChartYAMLDescription(content, desc)
        if (content !== chart.content) {
          const m = models.get('chart.yaml')
          chart.content = content
          if (m && m.getValue() !== content) m.setValue(content)
        }
      }
      fs.description = v.description
      fs.pools = v.pools
      fs.groups = v.groups
      fs.labels = v.labels

      // 2) 校验门禁：ERROR 阻断（已确认的决策），WARN 确认后放行。校验请求
      // 本身失败（网络/500/越权）同样阻断——失败不等于通过
      const ps = await opts.validate()
      if (!ps) {
        ElMessage.error('校验未能完成，已取消保存（内容未被提交）')
        return
      }
      const errors = ps.filter((p) => p.level === 'ERROR')
      if (errors.length) {
        ElMessage.error(`校验未通过：${errors.length} 个错误（已定位到文件，修正后再保存）`)
        return
      }
      const warns = ps.filter((p) => p.level === 'WARN')
      if (warns.length) {
        try {
          await ElMessageBox.confirm(`校验发现 ${warns.length} 个警告（见问题面板）。仍要保存？`, '警告', {
            confirmButtonText: '仍要保存', cancelButtonText: '回去修改', type: 'warning',
          })
        } catch { return }
      }

      // 3) 保存
      const body = fs.toSaveBody(v.version)
      if (opts.isCreate()) {
        const app = await api<{ ID: number; Name: string }>('POST', '/api/apps/spec', { ...body, name: fs.name })
        ElMessage.success(`应用 ${app.Name}@${v.version} 已创建`)
        await draftStore.clear()
        fs.markSaved(v.version)
        existingVersions.value.push(v.version)
        // URL 修正为编辑态（同路由 query 变化触发一次重建，重载新版本）
        void router.replace({ path: '/apps/ide', query: { app: String(app.ID), base: v.version } })
      } else {
        await api('PUT', `/api/apps/${opts.appId()}/spec`, body)
        ElMessage.success(`已保存版本 ${v.version}`)
        await draftStore.clear()
        fs.markSaved(v.version)
        existingVersions.value.push(v.version)
        draftStatus.value = ''
      }
    } catch (e) {
      ElMessage.error(`保存失败：${(e as Error).message}`)
    } finally {
      saving.value = false
    }
  }

  // ---- 离开抉择弹窗：主动导航离开且有未保存改动时当场问「暂存还是
  // 丢弃」，把去留决定收在离开那一刻（而不是留着等下次进来弹横幅）。
  // 横幅只留给异常中断（刷新/崩溃/会话失效）后的恢复场景。
  let leaveResolve: ((v: 'stash' | 'discard' | 'stay') => void) | null = null
  const leaveDialog = ref(false)
  function askLeaveChoice(): Promise<'stash' | 'discard' | 'stay'> {
    leaveDialog.value = true
    return new Promise((resolve) => { leaveResolve = resolve })
  }
  async function onLeaveChoice(v: 'stash' | 'discard' | 'stay') {
    leaveDialog.value = false
    if (leaveResolve) { leaveResolve(v); leaveResolve = null }
  }

  onBeforeRouteLeave(async (to) => {
    // 会话失效被强制送去登录：放行。拦截没有意义（会话已死），且自动
    // 暂存的 401 会反复触发本导航，形成弹框循环；这类异常中断正是
    // 重进时横幅恢复的适用场景
    if (to.path === '/user/login') return true
    if (!opts.isReady() || !opts.fs.isDirty()) return true
    const choice = await askLeaveChoice()
    if (choice === 'stay') return false
    if (choice === 'stash') {
      try {
        await opts.draftStore.save(
          opts.fs.toDraftPayload(opts.openTabs(), opts.activeTab(), 'deliberate'),
          opts.fs.baseVersion,
        )
      } catch (e) {
        ElMessage.error('暂存失败，已留在本页：' + (e as Error).message)
        return false
      }
    } else {
      await opts.draftStore.clear()
    }
    return true
  })

  // 卸载清理（自动暂存定时器），由组件 onBeforeUnmount 调用以保持原顺序
  function dispose() {
    clearTimeout(autosaveTimer)
  }

  return {
    draftInfo, draftStatus, saving, saveVisible, stashed,
    scheduleAutosave, manualDraft, discardDraft, setSuppressAutosave,
    onBeforeUnload, openSave, onSaveConfirm, leaveDialog, onLeaveChoice,
    dispose,
  }
}
