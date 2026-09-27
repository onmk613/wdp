// Monaco model / 标签页 / viewState 生命周期管理（自 AppIDE 抽取）：
//   · model 惰性创建（首次打开时），内容变化回写 fs 并回调调用方做联动
//   · tab 切换时保存/恢复 viewState（滚动位置、光标、选区）
//   · YAML 语义着色的防抖调度（decorIds 归 model 所有，随 model dispose）
// 纯机械管理，不含业务语义（暂存/保存/校验在 useDraftFlow/组件侧）。

import { ref } from 'vue'
import type { ShallowRef } from 'vue'
import { ElMessage } from 'element-plus'
import type * as Monaco from 'monaco-editor'
import type { ChartFS } from './fs'
import { languageFor, modelURI, type MonacoNs } from './monaco'
import { controlKeys, playKeysSet } from './keys'
import { applyDecorations } from './decorate'
import type { SchemaMeta } from '../api'

export interface EditorTabsOptions {
  fs: ChartFS
  monacoRef: ShallowRef<MonacoNs | null>
  // schema 元数据 getter（装饰的键集合由它派生；未加载时不调度）
  schemaMeta: () => SchemaMeta | null
  // model 内容变化联动（stashed 失效/装饰/域刷新/自动暂存），由调用方组装
  onContentChange: (path: string) => void
}

export function useEditorTabs(opts: EditorTabsOptions) {
  const tabs = ref<string[]>([])
  const active = ref('')
  const models = new Map<string, Monaco.editor.ITextModel>()
  const viewStates = new Map<string, ReturnType<NonNullable<Monaco.editor.IStandaloneCodeEditor['saveViewState']>>>()
  const decorIds = new Map<string, string[]>()

  // editor 实例在组件初始化链里创建（需要 monaco 加载完成与挂载的 DOM），
  // 创建后经 setEditor 注入；切换 tab 时用它保存/恢复 viewState
  let editor: Monaco.editor.IStandaloneCodeEditor | null = null
  function setEditor(e: Monaco.editor.IStandaloneCodeEditor) { editor = e }
  function getEditor() { return editor }

  function ensureModel(path: string): Monaco.editor.ITextModel | null {
    const monaco = opts.monacoRef.value
    const f = opts.fs.get(path)
    if (!monaco || !f) return null
    let m = models.get(path)
    if (m) return m
    m = monaco.editor.createModel(f.content, languageFor(path), modelURI(path))
    models.set(path, m)
    m.onDidChangeContent(() => {
      const file = opts.fs.get(path)
      if (file && !file.binary && file.content !== m!.getValue()) file.content = m!.getValue()
      opts.onContentChange(path)
    })
    scheduleDecorate(path)
    return m
  }

  function dropModel(path: string) {
    models.get(path)?.dispose()
    models.delete(path)
    viewStates.delete(path)
    decorIds.delete(path)
  }

  function openTab(path: string) {
    const f = opts.fs.get(path)
    if (!f || f.binary) {
      if (f?.binary) ElMessage.info(`${path} 是二进制文件（随包保留，不可编辑）`)
      return
    }
    if (!tabs.value.includes(path)) tabs.value.push(path)
    setActive(path)
  }

  function setActive(path: string) {
    if (active.value && editor) viewStates.set(active.value, editor.saveViewState())
    active.value = path
    const m = ensureModel(path)
    if (m && editor) {
      editor.setModel(m)
      const vs = viewStates.get(path)
      if (vs) editor.restoreViewState(vs)
      editor.focus()
    }
  }

  function closeTab(path: string) {
    const i = tabs.value.indexOf(path)
    if (i < 0) return
    tabs.value.splice(i, 1)
    viewStates.delete(path)
    if (active.value === path) {
      const next = tabs.value[Math.min(i, tabs.value.length - 1)] || ''
      active.value = ''
      if (next) setActive(next)
      else editor?.setModel(null)
    }
  }

  // ---- 语义着色（debounce） ----
  const decorTimers = new Map<string, number>()
  function scheduleDecorate(path: string) {
    const monaco = opts.monacoRef.value
    const meta = opts.schemaMeta()
    if (!monaco || !meta) return
    const old = decorTimers.get(path)
    if (old) clearTimeout(old)
    decorTimers.set(path, window.setTimeout(() => {
      const m = models.get(path)
      if (!m || !/\.ya?ml$/.test(path)) return
      const keys = { controlKeys: controlKeys(meta), playKeys: playKeysSet(meta) }
      decorIds.set(path, applyDecorations(monaco, m, decorIds.get(path) || [], keys))
    }, 350))
  }

  // ---- 卸载清理（与抽取前 AppIDE 的清理顺序一致，由组件 onBeforeUnmount 调用） ----
  // 装饰定时器逐个清掉：残留在已 dispose 的 model 上跑 applyDecorations
  // 会抛「Model is disposed」
  function clearTimers() {
    for (const t of decorTimers.values()) clearTimeout(t)
    decorTimers.clear()
  }
  function dispose() {
    editor?.dispose()
    for (const m of models.values()) m.dispose()
  }

  return {
    tabs, active, models,
    ensureModel, dropModel, openTab, setActive, closeTab, scheduleDecorate,
    setEditor, getEditor, clearTimers, dispose,
  }
}
