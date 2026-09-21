<script setup lang="ts">
// Chart IDE 主页面：左侧目录树 + 右侧多标签 Monaco 编辑区 + 底部
// 问题/文档面板。纯文本编辑（注释逐字保真），暂存（自动+手动）/
// 校验（门禁）/保存（版本化）三段式。
//
// 数据流：ChartFS 是唯一状态源；Monaco model 的内容变化回写 fs，
// 文件操作（树）改 fs 后同步 model/标签。保存与校验体由 fs 统一序列化。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { onBeforeRouteLeave, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowLeft, RefreshLeft } from '@element-plus/icons-vue'
import type * as Monaco from 'monaco-editor'
import {
  api, type AppSpec, type GroupEntry, type ModuleMeta, type Pool, type SchemaMeta,
} from '../api'
import {
  ChartFS, chartYAMLDescription, chartYAMLVersion, nextVersion,
  patchChartYAMLDescription, patchChartYAMLVersion, type DraftPayload,
} from '../ide/fs'
import { loadMonaco, editorOptions, bindSuggestKey, languageFor, modelURI, type MonacoNs } from '../ide/monaco'
import { applyYamlSchemas, controlKeys, fetchSchema } from '../ide/schemas'
import { registerProviders } from '../ide/complete'
import { buildVarDomain, type VarDomain } from '../ide/vars'
import { DraftStore } from '../ide/draft'
import { applyMarkers, runValidate, type ProblemItem } from '../ide/validate'
import { applyDecorations } from '../ide/decorate'
import FileTree from '../components/ide/FileTree.vue'
import BottomPanel from '../components/ide/BottomPanel.vue'
import SaveDialog from '../components/ide/SaveDialog.vue'

const props = defineProps<{
  appId: number // 0 = 新建
  baseVersion?: string
  newName?: string
  newVersion?: string
  newDesc?: string
  restore?: boolean // 草稿箱「继续编辑」直达：进页即恢复草稿，不走确认横幅
}>()
const router = useRouter()

const loading = ref(true)
const fs = new ChartFS()
const monacoRef = shallowRef<MonacoNs | null>(null)
const editorRef = ref<HTMLElement>()
let editor: Monaco.editor.IStandaloneCodeEditor | null = null
// 补全 provider 的释放句柄：注册在 monaco 语言层全局生效，卸载必须
// dispose（不释放则路由往返叠加，候选出现多份）
let providers: Monaco.IDisposable | null = null
const models = new Map<string, Monaco.editor.ITextModel>()
const viewStates = new Map<string, ReturnType<NonNullable<Monaco.editor.IStandaloneCodeEditor['saveViewState']>>>()
const decorIds = new Map<string, string[]>()

// 标签页
const tabs = ref<string[]>([])
const active = ref('')

// 元数据
const schemaMeta = shallowRef<SchemaMeta | null>(null)
const modulesMeta = shallowRef<ModuleMeta[] | null>(null)
const domain = shallowRef<VarDomain | null>(null)
const hostGroups = shallowRef<{ hosts: string[]; groups: string[] } | null>(null)
const pools = ref<Pool[]>([])
const groups = ref<GroupEntry[]>([])
const existingVersions = ref<string[]>([])

// 问题与面板
const problems = ref<ProblemItem[]>([])
const panel = ref<'none' | 'problems' | 'doc'>('none')
const cursorModule = ref<ModuleMeta | null>(null)

// 暂存与保存
const isCreate = computed(() => props.appId === 0)
const draftStore = new DraftStore(isCreate.value ? `new:${props.newName || ''}` : String(props.appId))
const draftInfo = ref<{ payload: DraftPayload; updated_at: string } | null>(null)
const draftStatus = ref('')
const saving = ref(false)
const saveVisible = ref(false)

// 变更版本号（任何 fs/model 内容或文件操作都推进，驱动自动暂存与域刷新）
const fsVersion = ref(0)
const dirtyCount = computed(() => fs.dirtyCount())

// 左侧目录树宽度可拖拽调整（分隔条 mousedown → document mousemove）
const treeWidth = ref(240)
const dragging = ref(false)
function onSplitterDown(e: MouseEvent) {
  dragging.value = true
  e.preventDefault()
}
function onSplitterMove(e: MouseEvent) {
  if (!dragging.value) return
  // 树容器起点近似为 0（Console 主内容区内），用 clientX 直接换算
  const root = document.querySelector('.ide-root') as HTMLElement | null
  if (!root) return
  const left = root.getBoundingClientRect().left
  treeWidth.value = Math.min(460, Math.max(170, e.clientX - left - 12))
}
function onSplitterUp() {
  dragging.value = false
}
onMounted(() => {
  document.addEventListener('mousemove', onSplitterMove)
  document.addEventListener('mouseup', onSplitterUp)
  window.addEventListener('wdp-unauthorized', onUnauthorizedEvt)
  window.addEventListener('focus', onWinFocus)
})
onBeforeUnmount(() => {
  document.removeEventListener('mousemove', onSplitterMove)
  document.removeEventListener('mouseup', onSplitterUp)
  window.removeEventListener('wdp-unauthorized', onUnauthorizedEvt)
  window.removeEventListener('focus', onWinFocus)
})

const title = computed(() =>
  isCreate.value ? `新建应用 ${fs.name || props.newName}` : `应用 ${fs.name}`,
)
const subtitle = computed(() =>
  isCreate.value ? '保存后正式入库' : `底本 ${fs.baseVersion} · 保存即生成新版本（版本不可覆盖）`,
)

// ---- Monaco model 管理 ----
function ensureModel(path: string): Monaco.editor.ITextModel | null {
  const monaco = monacoRef.value
  const f = fs.get(path)
  if (!monaco || !f) return null
  let m = models.get(path)
  if (m) return m
  m = monaco.editor.createModel(f.content, languageFor(path), modelURI(path))
  models.set(path, m)
  m.onDidChangeContent(() => {
    const file = fs.get(path)
    if (file && !file.binary && file.content !== m!.getValue()) file.content = m!.getValue()
    onContentChanged(path)
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
  const f = fs.get(path)
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

// ---- 内容变化联动（debounce） ----
let decorTimers = new Map<string, number>()
function scheduleDecorate(path: string) {
  const monaco = monacoRef.value
  if (!monaco || !schemaMeta.value) return
  const old = decorTimers.get(path)
  if (old) clearTimeout(old)
  decorTimers.set(path, window.setTimeout(() => {
    const m = models.get(path)
    if (!m || !/\.ya?ml$/.test(path)) return
    const keys = { controlKeys: ctlKeys(), playKeys: playKeysSet() }
    decorIds.set(path, applyDecorations(monaco, m, decorIds.get(path) || [], keys))
  }, 350))
}

let domainTimer = 0
function scheduleDomain() {
  clearTimeout(domainTimer)
  domainTimer = window.setTimeout(rebuildDomain, 400)
}

function rebuildDomain() {
  if (!schemaMeta.value) return
  const fileMap = new Map<string, string>()
  for (const f of fs.allFiles()) {
    if (f.deleted || f.binary) continue
    fileMap.set(f.path, models.get(f.path)?.getValue() ?? f.content)
  }
  domain.value = buildVarDomain(fileMap, schemaMeta.value)
}

function onContentChanged(path: string) {
  fsVersion.value++
  stashed.value = false // 改动未暂存
  if (/\.ya?ml$/.test(path)) scheduleDecorate(path)
  scheduleDomain()
  scheduleAutosave()
}

// 根目录 yaml 集合变化（新相位/重命名）→ 重新装配 schema
const rootYamlKey = computed(() =>
  fs.allFiles().filter((f) => !f.deleted && /^([A-Za-z0-9_-]+)\.yaml$/.test(f.path)).map((f) => f.path).sort().join(','),
)
watch(rootYamlKey, () => { applySchemas() })

// values.schema.json 内容变化 → values.yaml 的实时校验即时跟随。此前只
// 监听文件集合变化，编辑 schema 文件本身不重装配，values.yaml 一直按打
// 开时的旧 schema 校验，直到刷新页面
let schemaTimer = 0
const valuesSchemaKey = computed(() => {
  const f = fs.get('values.schema.json')
  return f && !f.deleted && !f.binary ? f.content : ''
})
watch(valuesSchemaKey, () => {
  clearTimeout(schemaTimer)
  schemaTimer = window.setTimeout(applySchemas, 400)
})

function ctlKeys(): Set<string> {
  return schemaMeta.value ? controlKeys(schemaMeta.value) : new Set()
}
function playKeysSet(): Set<string> {
  const s = new Set<string>()
  if (!schemaMeta.value) return s
  for (const sec of schemaMeta.value.play) for (const f of sec.Fields) s.add(f.Name)
  s.add('tasks')
  s.add('handlers')
  return s
}

function applySchemas() {
  const meta = schemaMeta.value
  if (!meta) return
  const paths = fs.allFiles().filter((f) => !f.deleted).map((f) => f.path)
  const valuesSchema = fs.get('values.schema.json')?.content
  applyYamlSchemas(paths, meta, valuesSchema)
}

// ---- 文件操作（树 → model/标签同步 + 撤销栈） ----
interface FileOp { kind: string; path: string; to?: string; prevContent?: string }
const fileOps = ref<FileOp[]>([])

function onTreeOp(ev: { kind: string; path: string; to?: string }) {
  if (ev.kind === 'create') {
    ensureModel(ev.path)
  } else if (ev.kind === 'edit') {
    // chart.yaml 被树操作（相位声明）改写：同步 model
    const m = models.get(ev.path)
    const f = fs.get(ev.path)
    if (m && f && m.getValue() !== f.content) m.setValue(f.content)
  } else if (ev.kind === 'rename') {
    const from = ev.path, to = ev.to!
    dropModel(from)
    const f = fs.get(to)
    const i = tabs.value.indexOf(from)
    if (i >= 0) tabs.value.splice(i, 1, to)
    if (f) {
      const nm = ensureModel(to)
      if (nm) nm.setValue(f.content)
    }
    if (active.value === from) active.value = to
    if (f && !tabs.value.includes(to)) tabs.value.push(to)
    ElMessage.success(`已重命名 ${from} → ${to}`)
  } else if (ev.kind === 'delete') {
    dropModel(ev.path)
    closeTab(ev.path)
  } else if (ev.kind === 'restore') {
    ensureModel(ev.path)
  }
  fileOps.value.push(ev as FileOp)
  fsVersion.value++
  stashed.value = false
  scheduleDomain()
  scheduleAutosave()
}

// 文件操作撤销（焦点不在编辑器时 Ctrl+Z）
function undoFileOp() {
  const op = fileOps.value.pop()
  if (!op) return
  if (op.kind === 'create') {
    fs.remove(op.path)
    dropModel(op.path)
    closeTab(op.path)
  } else if (op.kind === 'delete') {
    fs.restore(op.path)
    ensureModel(op.path)
  } else if (op.kind === 'rename') {
    const err = fs.rename(op.to!, op.path)
    if (err) {
      fileOps.value.push(op) // 撤销失败（如旧路径被占用）：操作保留可重试
      ElMessage.warning(`撤销重命名失败：${err}`)
      return
    }
    onTreeOp({ kind: 'rename', path: op.to!, to: op.path })
    fileOps.value.pop() // 撤销不入栈
  } else if (op.kind === 'edit') {
    // 树操作对文件的程序化改写（相位声明写 chart.yaml）：按快照还原
    if (op.prevContent !== undefined) {
      const f = fs.get(op.path)
      if (f) {
        f.content = op.prevContent
        const m = models.get(op.path)
        if (m && m.getValue() !== op.prevContent) m.setValue(op.prevContent)
      }
    }
  }
  fsVersion.value++
  stashed.value = false
  scheduleDomain()
  scheduleAutosave()
}

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

function onDocKeydown(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z') {
    const t = e.target as HTMLElement
    if (t.closest('.monaco-editor') || t.closest('input,textarea,[contenteditable]')) return
    if (!fileOps.value.length) return
    e.preventDefault()
    undoFileOp()
  }
}

// ---- 暂存 ----
let autosaveTimer = 0
let suppressAutosave = false
function scheduleAutosave() {
  if (suppressAutosave) return
  clearTimeout(autosaveTimer)
  autosaveTimer = window.setTimeout(autoDraft, 2500)
}
async function autoDraft() {
  if (!fs.isDirty() || suppressAutosave || loading.value || authDead.value) return
  try {
    await draftStore.save(fs.toDraftPayload(tabs.value, active.value), fs.baseVersion)
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
    await draftStore.save(fs.toDraftPayload(tabs.value, active.value), fs.baseVersion)
    stashed.value = true
    draftStatus.value = `已暂存 ${new Date().toLocaleTimeString('zh-CN', { hour12: false })}`
    ElMessage.success('已暂存（跨设备可恢复）')
  } catch (e) {
    ElMessage.error(`暂存失败：${(e as Error).message}`)
  }
}

// 草稿恢复：整体重建 fs 与 models
function restoreDraft(payload: DraftPayload) {
  suppressAutosave = true
  for (const p of [...models.keys()]) dropModel(p)
  fs.loadFromDraft(payload)
  for (const f of fs.textFiles()) ensureModel(f.path)
  tabs.value = (payload.open_tabs || []).filter((p) => fs.get(p) && !fs.get(p)!.binary)
  if (!tabs.value.length) tabs.value = ['deploy.yaml']
  active.value = ''
  setActive(payload.active_tab && tabs.value.includes(payload.active_tab) ? payload.active_tab : tabs.value[0])
  applySchemas()
  rebuildDomain()
  fileOps.value = []
  draftInfo.value = null
  suppressAutosave = false
  stashed.value = true // 恢复的内容本身来自暂存
  fsVersion.value++
}

async function onRestoreFromBanner() {
  const payload = draftInfo.value?.payload
  if (!payload) return
  restoreDraft(payload)
  ElMessage.success('草稿已恢复')
}

async function discardDraft() {
  await draftStore.clear()
  draftInfo.value = null
}

// ---- 校验 ----
const validating = ref(false)
// 失败返回 null（区别于「校验通过、无发现」的 []）：保存门禁据它中止——
// 此前失败被吞成 []，网络错误/500/越权都会被当成「校验通过」直接放行保存
async function doValidate(): Promise<ProblemItem[] | null> {
  const version = currentChartVersion()
  validating.value = true
  try {
    const body = fs.toSaveBody(version)
    const ps = await runValidate(body, isCreate.value ? 0 : props.appId)
    problems.value = ps
    applyMarkers(monacoRef.value!, models, ps)
    if (ps.length) panel.value = 'problems'
    return ps
  } catch (e) {
    ElMessage.error(`校验失败：${(e as Error).message}`)
    return null
  } finally {
    validating.value = false
  }
}

function currentChartVersion(): string {
  const c = fs.get('chart.yaml')?.content || ''
  const v = chartYAMLVersion(c)
  if (v && (isCreate.value || !existingVersions.value.includes(v))) return v
  return nextVersion(fs.baseVersion, existingVersions.value)
}

// ---- 保存 ----
function openSave() {
  if (!fs.isDirty() && !isCreate.value) {
    ElMessage.info('没有改动')
    return
  }
  saveVisible.value = true
}

async function onSaveConfirm(v: { version: string; description: string; pools: string[]; groups: string[]; labels: string }) {
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
  const ps = await doValidate()
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
  saving.value = true
  try {
    const body = fs.toSaveBody(v.version)
    if (isCreate.value) {
      const app = await api<{ ID: number; Name: string }>('POST', '/api/apps/spec', { ...body, name: fs.name })
      ElMessage.success(`应用 ${app.Name}@${v.version} 已创建`)
      await draftStore.clear()
      fs.markSaved(v.version)
      existingVersions.value.push(v.version)
      // URL 修正为编辑态（同路由 query 变化触发一次重建，重载新版本）
      void router.replace({ path: '/apps/ide', query: { app: String(app.ID), base: v.version } })
    } else {
      await api('PUT', `/api/apps/${props.appId}/spec`, body)
      ElMessage.success(`已保存版本 ${v.version}`)
      await draftStore.clear()
      fs.markSaved(v.version)
      existingVersions.value.push(v.version)
      draftStatus.value = ''
      fsVersion.value++
    }
  } catch (e) {
    ElMessage.error(`保存失败：${(e as Error).message}`)
  } finally {
    saving.value = false
  }
}

// ---- 光标联动文档面板 ----
function onCursorChange(e: Monaco.editor.ICursorPositionChangedEvent) {
  cursorPos.value = { line: e.position.lineNumber, col: e.position.column }
  const monaco = monacoRef.value
  const m = editor?.getModel()
  if (!monaco || !m) return
  const text = m.getValue()
  const lines = text.split('\n')
  const line = lines[e.position.lineNumber - 1] || ''
  // 从当前行向上找最近的模块键（同任务块内）
  const ctl = ctlKeys()
  const pk = playKeysSet()
  const keyRe = /^(\s*)(-\s+)?([\w.][\w-]*)\s*:/
  let found: string | null = null
  const indent = line.match(/^ */)![0].length
  for (let i = e.position.lineNumber - 1; i >= 0 && i >= e.position.lineNumber - 60; i--) {
    const l = lines[i] || ''
    const mm = l.match(keyRe)
    if (!mm) continue
    const li = mm[1].length + (mm[2] ? mm[2].length : 0)
    if (i === e.position.lineNumber - 1 || li <= indent) {
      const k = mm[3]
      if (!ctl.has(k) && !pk.has(k) && k !== 'name' && k !== 'tasks' && k !== 'handlers') {
        found = k
        break
      }
      if (pk.has(k)) break // 到 play 级了
    }
  }
  cursorModule.value = (found && modulesMeta.value?.find((x) => x.name === found)) || null
}

// ---- 初始化 ----
let initialized = false
// 卸载标记：onMounted 是长异步链（monaco 加载 + 多个网络请求），快速
// 进出路由时 await 之后仍会继续执行——若已卸载还创建 editor/models/
// providers，清理钩子早已跑过，资源净泄漏。每个 await 后检查。
let disposed = false
const invalidCreate = isCreate.value && !props.newName
onMounted(async () => {
  if (invalidCreate) {
    // 无名直达（手输 URL/旧收藏）：回新建入口。否则草稿键变成 new:，
    // 服务端 400，自动暂存每 2.5s 失败刷屏
    loading.value = false
    ElMessage.info('请先填写应用名称')
    void router.replace('/apps/new')
    return
  }
  document.addEventListener('keydown', onDocKeydown)
  window.addEventListener('beforeunload', onBeforeUnload)
  try {
    const monaco = await loadMonaco()
    if (disposed) return
    monacoRef.value = monaco

    const [schema, mods, ps, gs] = await Promise.all([
      fetchSchema(),
      api<ModuleMeta[]>('GET', '/api/modules'),
      api<Pool[]>('GET', '/api/pools'),
      api<GroupEntry[]>('GET', '/api/groups'),
    ])
    if (disposed) return
    schemaMeta.value = schema
    modulesMeta.value = mods
    pools.value = ps
    groups.value = gs

    // 主机/组名（hosts 值补全）
    try {
      const [hosts, hgroups] = await Promise.all([
        api<{ Name: string }[]>('GET', '/api/hosts'),
        api<{ Name: string }[]>('GET', '/api/groups'),
      ])
      if (disposed) return
      hostGroups.value = {
        hosts: hosts.map((h) => h.Name),
        groups: hgroups.map((g) => g.Name),
      }
    } catch { hostGroups.value = null }

    // 加载内容：编辑模式读 spec；新建模式脚手架
    if (!isCreate.value) {
      const detail = await api<{ app: { LatestVersion: string }; versions: { Version: string }[] }>(
        'GET', `/api/apps/${props.appId}`,
      )
      if (disposed) return
      existingVersions.value = (detail.versions || []).map((v) => v.Version)
      const q = props.baseVersion ? `?version=${encodeURIComponent(props.baseVersion)}` : ''
      const spec = await api<AppSpec>('GET', `/api/apps/${props.appId}/spec${q}`)
      if (disposed) return
      fs.loadFromSpec(spec, spec.version)
    } else {
      fs.loadFromScaffold(props.newName || 'myapp', props.newVersion || '1.0.0', props.newDesc || '')
    }

    // 编辑器与 model
    await nextTick()
    if (disposed) return
    editor = monaco.editor.create(editorRef.value!, {
      ...editorOptions(monaco),
      glyphMargin: true,
    })
    bindSuggestKey(monaco, editor)
    editor.onDidChangeCursorPosition(onCursorChange)
    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, openSave)
    for (const f of fs.textFiles()) ensureModel(f.path)

    providers = registerProviders(monaco, {
      fs,
      meta: () => schemaMeta.value,
      modules: () => modulesMeta.value,
      domain: () => domain.value,
      hostGroups: () => hostGroups.value,
    })
    applySchemas()
    rebuildDomain()
    for (const f of fs.textFiles()) scheduleDecorate(f.path)

    tabs.value = ['deploy.yaml', 'values.yaml', 'chart.yaml'].filter((p) => fs.get(p))
    setActive('deploy.yaml')
    loading.value = false
    initialized = true

    // 草稿检测：异常中断（自动暂存）→ 横幅让用户决定；主动暂存离开 →
    // 底本没变就静默续上（用户已经决定保留），底本变了才询问；
    // 草稿箱「继续编辑」（?restore=1）→ 无条件静默恢复
    const draft = await draftStore.load()
    if (disposed) return
    if (draft && draftDiffers(draft.payload)) {
      const sameBase = (draft.payload.base_version || '') === fs.baseVersion
      if ((draft.payload.mode === 'deliberate' && sameBase) || props.restore) {
        restoreDraft(draft.payload)
        ElMessage.success('已续上上次暂存的修改')
      } else {
        draftInfo.value = { payload: draft.payload, updated_at: draft.updated_at }
      }
    }
  } catch (e) {
    loading.value = false
    ElMessage.error(`加载失败：${(e as Error).message}`)
  }
})

function draftDiffers(p: DraftPayload): boolean {
  // 文件集合或内容有差异即算（UI 状态不算）
  const pick = (d: DraftPayload) => JSON.stringify([d.files, d.deleted_files, d.name, d.base_version])
  return pick(p) !== pick(fs.toDraftPayload(tabs.value, active.value))
}

function onBeforeUnload(e: BeforeUnloadEvent) {
  if (fs.isDirty()) {
    e.preventDefault()
    e.returnValue = ''
  }
}

// 离开抉择弹窗：主动导航离开且有未保存改动时当场问「暂存还是丢弃」，
// 把去留决定收在离开那一刻（而不是留着等下次进来弹横幅）。横幅只留给
// 异常中断（刷新/崩溃/会话失效）后的恢复场景。
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
  if (!initialized || !fs.isDirty()) return true
  const choice = await askLeaveChoice()
  if (choice === 'stay') return false
  if (choice === 'stash') {
    try {
      await draftStore.save(fs.toDraftPayload(tabs.value, active.value, 'deliberate'), fs.baseVersion)
    } catch (e) {
      ElMessage.error('暂存失败，已留在本页：' + (e as Error).message)
      return false
    }
  } else {
    await draftStore.clear()
  }
  return true
})

onBeforeUnmount(() => {
  disposed = true
  document.removeEventListener('keydown', onDocKeydown)
  window.removeEventListener('beforeunload', onBeforeUnload)
  clearTimeout(autosaveTimer)
  clearTimeout(domainTimer)
  clearTimeout(schemaTimer)
  // 装饰定时器逐个清掉：残留在已 dispose 的 model 上跑 applyDecorations
  // 会抛「Model is disposed」
  for (const t of decorTimers.values()) clearTimeout(t)
  decorTimers.clear()
  providers?.dispose()
  providers = null
  editor?.dispose()
  for (const m of models.values()) m.dispose()
})

// 状态栏
const cursorPos = ref({ line: 1, col: 1 })
watch(active, () => { cursorPos.value = { line: 1, col: 1 } })

function jumpTo(path: string, line?: number) {
  openTab(path)
  if (line && editor) {
    nextTick(() => editor?.revealLineInCenter(line))
    nextTick(() => editor?.setPosition({ lineNumber: line, column: 1 }))
  }
  panel.value = 'problems'
}

function back() {
  void router.push('/apps')
}

// suggestedVersion：chart.yaml 里的版本未被占用则用之，否则推进
const suggestedVersion = computed(() => currentChartVersion())
const defaultDescription = computed(() => {
  const c = fs.get('chart.yaml')?.content || ''
  return chartYAMLDescription(c) || fs.description
})
</script>

<template>
  <div v-loading="loading" class="ide-root">
    <!-- 顶栏 -->
    <div class="ide-top">
      <el-button text :icon="ArrowLeft" @click="back">返回</el-button>
      <b class="ide-title">{{ title }}</b>
      <span class="muted">{{ subtitle }}</span>
      <span v-if="dirtyCount" class="dirty">● {{ dirtyCount }} 处未保存</span>
      <span v-else class="saved muted">内容与底本一致</span>
      <div style="flex: 1" />
      <span class="muted draft-state">{{ draftStatus }}</span>
      <el-tooltip content="撤销上一步文件操作（Ctrl+Z 焦点不在编辑器时）" placement="bottom">
        <el-button :icon="RefreshLeft" :disabled="!fileOps.length" @click="undoFileOp" />
      </el-tooltip>
      <el-button @click="manualDraft">暂存</el-button>
      <el-button :loading="validating" @click="doValidate">校验</el-button>
      <el-button type="primary" :loading="saving" @click="openSave">保存</el-button>
    </div>

    <!-- 草稿恢复横幅 -->
    <el-alert v-if="draftInfo" type="warning" :closable="false" class="draft-banner">
      <template #title>
        检测到 {{ new Date(draftInfo.updated_at).toLocaleString() }} 的未恢复草稿
        <el-button size="small" type="primary" @click="onRestoreFromBanner">恢复草稿</el-button>
        <el-button size="small" @click="discardDraft">丢弃</el-button>
      </template>
    </el-alert>

    <!-- 主体 -->
    <div class="ide-body">
      <div class="ide-tree" :style="{ width: treeWidth + 'px' }">
        <FileTree :fs="fs" :problems="problems" @open="openTab" @op="onTreeOp" />
      </div>
      <div class="ide-splitter" :class="{ dragging: dragging }" title="拖拽调整宽度" @mousedown="onSplitterDown" />
      <div class="ide-main">
        <div class="ide-tabs">
          <div
            v-for="t in tabs" :key="t"
            class="tab" :class="{ active: t === active }"
            @click="setActive(t)" @mousedown.middle.prevent="closeTab(t)"
          >
            <span class="tab-label">{{ t.split('/').pop() }}</span>
            <span
              v-if="fs.get(t) && fs.get(t)!.content !== fs.get(t)!.original"
              class="tab-dirty" title="未保存"
            >●</span>
            <span class="tab-close" title="关闭" @click.stop="closeTab(t)">×</span>
          </div>
          <div style="flex: 1" />
          <el-radio-group v-model="panel" size="small" class="panel-toggle">
            <el-radio-button value="none">▾</el-radio-button>
            <el-radio-button value="problems">问题</el-radio-button>
            <el-radio-button value="doc">文档</el-radio-button>
          </el-radio-group>
        </div>
        <div class="ide-editor-wrap">
          <div ref="editorRef" class="ide-editor" />
          <BottomPanel
            v-if="panel !== 'none'"
            v-model:panel="panel"
            :problems="problems"
            :module="cursorModule"
            @jump="jumpTo"
          />
        </div>
        <div class="ide-status">
          <span class="muted">{{ active || '（未打开文件）' }}</span>
          <span class="muted">Ln {{ cursorPos.line }}, Col {{ cursorPos.col }}</span>
          <span class="muted">2 空格缩进</span>
          <span v-if="problems.filter((p) => p.level === 'ERROR').length" class="err-count">
            ✗ {{ problems.filter((p) => p.level === 'ERROR').length }} 错误
          </span>
          <span class="grow" />
          <span class="muted">Ctrl+S 保存 · Ctrl+Z 撤销 · 模块名/参数/{{ '{{' }} 变量自动补全 · Alt+Space 手动触发</span>
        </div>
      </div>
    </div>

    <!-- 离开抉择：主动导航离开且有未保存改动 -->
    <el-dialog :model-value="leaveDialog" title="离开前处理未保存的修改" width="460px"
      :close-on-click-modal="false" @update:model-value="(v: boolean) => { if (!v) onLeaveChoice('stay') }">
      <p style="margin: 0 0 4px">有未保存的修改（相对底本 {{ fs.baseVersion || '（新建）' }}）。</p>
      <p class="muted" style="margin: 0">暂存：跨设备可恢复，重新进入本应用时自动续上；丢弃：不留草稿，回到已保存版本。</p>
      <template #footer>
        <el-button @click="onLeaveChoice('stay')">取消（留在本页）</el-button>
        <el-button type="danger" plain @click="onLeaveChoice('discard')">丢弃修改并离开</el-button>
        <el-button type="primary" @click="onLeaveChoice('stash')">暂存并离开</el-button>
      </template>
    </el-dialog>

    <SaveDialog
      v-model:visible="saveVisible"
      :is-create="isCreate"
      :suggested-version="suggestedVersion"
      :default-description="defaultDescription"
      :taken-versions="existingVersions"
      :pools="pools"
      :groups="groups"
      :current="{ pools: fs.pools, groups: fs.groups, labels: fs.labels }"
      @confirm="onSaveConfirm"
    />
  </div>
</template>

<style>
/* Monaco 语义着色（非 scoped：decorations 注入的类需要全局可见） */
.wdp-mod-key { color: #1d4ed8 !important; font-weight: 600; }
.wdp-ctl-key { color: #b45309 !important; }
.wdp-play-key { color: #0f766e !important; }
.wdp-badge-when::before { content: '?'; color: #d97706; font-weight: 700; font-size: 11px; }
.wdp-badge-loop::before { content: '↻'; color: #7c3aed; font-weight: 700; font-size: 11px; }
.wdp-badge-reg::before { content: '→'; color: #059669; font-weight: 700; font-size: 11px; }
</style>

<style scoped>
.ide-root {
  display: flex;
  flex-direction: column;
  height: calc(100vh - 116px);
  min-height: 480px;
  background: #fff;
  border: 1px solid #e4e7ed;
  border-radius: 8px;
  overflow: hidden;
}
.ide-top {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 12px;
  border-bottom: 1px solid #e4e7ed;
  flex: none;
}
.ide-title { font-size: 14px; }
.dirty { color: #d97706; font-size: 12px; }
.saved { font-size: 12px; }
.draft-state { font-size: 12px; }
.draft-banner { border-radius: 0; flex: none; }
.ide-body {
  display: flex;
  flex: 1;
  min-height: 0;
}
.ide-tree {
  flex: none;
  border-right: none;
  min-height: 0;
  min-width: 170px;
  max-width: 460px;
}
.ide-splitter {
  flex: none;
  width: 5px;
  cursor: col-resize;
  background: transparent;
  border-right: 1px solid #ebeef5;
  transition: background 0.12s;
}
.ide-splitter:hover,
.ide-splitter.dragging {
  background: #c7d7f5;
}
.ide-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
}
.ide-tabs {
  display: flex;
  align-items: center;
  background: #f8fafc;
  border-bottom: 1px solid #e4e7ed;
  padding: 0 6px;
  height: 36px;
  flex: none;
  overflow-x: auto;
}
.tab {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 0 10px;
  height: 28px;
  font-size: 12.5px;
  border-radius: 6px 6px 0 0;
  cursor: pointer;
  color: #64748b;
  white-space: nowrap;
  user-select: none;
}
.tab.active {
  background: #fff;
  color: #1d4ed8;
  border: 1px solid #e4e7ed;
  border-bottom-color: #fff;
}
.tab-dirty { color: #d97706; font-size: 10px; }
.tab-close {
  color: #94a3b8;
  font-size: 13px;
  padding: 0 2px;
  border-radius: 3px;
}
.tab-close:hover { background: #e2e8f0; color: #334155; }
.panel-toggle { margin-right: 6px; }
.ide-editor-wrap {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.ide-editor {
  flex: 1;
  min-height: 200px;
}
.ide-status {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 3px 12px;
  border-top: 1px solid #e4e7ed;
  background: #fafbfd;
  flex: none;
  font-size: 12px;
}
.err-count { color: #dc2626; }
.grow { flex: 1; }
.muted { color: #909399; font-size: 12px; }
</style>
