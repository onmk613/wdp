<script setup lang="ts">
// Chart IDE 主页面：左侧目录树 + 右侧多标签 Monaco 编辑区 + 底部
// 问题/文档面板。纯文本编辑（注释逐字保真），暂存（自动+手动）/
// 校验（门禁）/保存（版本化）三段式。
//
// 数据流：ChartFS 是唯一状态源；Monaco model 的内容变化回写 fs，
// 文件操作（树）改 fs 后同步 model/标签。保存与校验体由 fs 统一序列化。
//
// 结构：model/标签/viewState 生命周期在 ide/useEditorTabs；暂存-保存
// 状态机与离开路由守卫在 ide/useDraftFlow；文件操作与撤销栈在
// ide/useFileOps。本组件保留初始化链、schema/变量域联动、校验与问题
// 面板、光标文档联动，以及草稿恢复的整体重建（跨三者，是集成点）。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { ArrowLeft, RefreshLeft } from '@element-plus/icons-vue'
import type * as Monaco from 'monaco-editor'
import {
  api, type AppSpec, type GroupEntry, type ModuleMeta, type Pool, type SchemaMeta,
} from '../api'
import {
  ChartFS, chartYAMLDescription, chartYAMLVersion, nextVersion, type DraftPayload,
} from '../ide/fs'
import { loadMonaco, editorOptions, bindSuggestKey, type MonacoNs } from '../ide/monaco'
import { applyYamlSchemas, fetchSchema } from '../ide/schemas'
import { controlKeys, playKeysSet } from '../ide/keys'
import { registerProviders } from '../ide/complete'
import { buildVarDomain, type VarDomain } from '../ide/vars'
import { DraftStore } from '../ide/draft'
import { applyMarkers, runValidate, type ProblemItem } from '../ide/validate'
import FileTree from '../components/ide/FileTree.vue'
import BottomPanel from '../components/ide/BottomPanel.vue'
import SaveDialog from '../components/ide/SaveDialog.vue'
import { useEditorTabs } from '../ide/useEditorTabs'
import { useDraftFlow } from '../ide/useDraftFlow'
import { useFileOps } from '../ide/useFileOps'

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
// 补全 provider 的释放句柄：注册在 monaco 语言层全局生效，卸载必须
// dispose（不释放则路由往返叠加，候选出现多份）
let providers: Monaco.IDisposable | null = null

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
// 跑过至少一次整体校验（问题面板空态据此区分「还没跑」与「跑过且干净」）
const validated = ref(false)
const panel = ref<'none' | 'problems' | 'doc'>('none')
const cursorModule = ref<ModuleMeta | null>(null)

// 暂存与保存（状态机本体在 useDraftFlow，这里只保留构造参数与校验态）
const isCreate = computed(() => props.appId === 0)
const draftStore = new DraftStore(isCreate.value ? `new:${props.newName || ''}` : String(props.appId))
const validating = ref(false)

// 变更版本号（任何 fs/model 内容或文件操作都推进，驱动自动暂存与域刷新）
const dirtyCount = computed(() => fs.dirtyCount())

// ---- Monaco model/标签/viewState 生命周期 ----
const tabsApi = useEditorTabs({
  fs,
  monacoRef,
  schemaMeta: () => schemaMeta.value,
  // onContentChanged 是下方的函数声明（提升），model 内容变化时才调用
  onContentChange: onContentChanged,
})
const {
  tabs, active, models, ensureModel, dropModel, openTab, setActive, closeTab, scheduleDecorate,
  setEditor, getEditor, clearTimers: clearDecorTimers, dispose: disposeTabs,
} = tabsApi

// ---- 暂存-保存状态机与离开守卫 ----
const {
  draftInfo, draftStatus, saving, saveVisible, stashed,
  scheduleAutosave, manualDraft, discardDraft, setSuppressAutosave,
  onBeforeUnload, openSave, onSaveConfirm, leaveDialog, onLeaveChoice,
  dispose: disposeDraft,
} = useDraftFlow({
  fs,
  draftStore,
  validating,
  loading,
  isCreate: () => isCreate.value,
  appId: () => props.appId,
  openTabs: () => tabs.value,
  activeTab: () => active.value,
  models,
  existingVersions,
  // doValidate 是函数声明（提升）：校验本体留在组件（问题面板/markers 归这）
  validate: doValidate,
  isReady: () => initialized,
})

// ---- 文件操作与撤销栈 ----
const { fileOps, onTreeOp, undoFileOp, onDocKeydown } = useFileOps({
  fs,
  tabsApi,
  // onFsMutated 同为提升的函数声明
  onMutate: onFsMutated,
})

// 左侧目录树宽度可拖拽调整（分隔条 mousedown → document mousemove）
const treeWidth = ref(240)
const dragging = ref(false)
function onSplitterDown(e: MouseEvent) {
  dragging.value = true
  e.preventDefault()
}
function onSplitterMove(e: MouseEvent) {
  if (!dragging.value) return
  // 按 .ide-root 实际左缘换算，兼容布局偏移
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
})
onBeforeUnmount(() => {
  document.removeEventListener('mousemove', onSplitterMove)
  document.removeEventListener('mouseup', onSplitterUp)
})

const title = computed(() =>
  isCreate.value ? `新建应用 ${fs.name || props.newName}` : `应用 ${fs.name}`,
)
const subtitle = computed(() =>
  isCreate.value ? '保存后正式入库' : `底本 ${fs.baseVersion} · 保存即生成新版本（版本不可覆盖）`,
)

// ---- 变量域（debounce 重建） ----
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
  stashed.value = false // 改动未暂存
  if (/\.ya?ml$/.test(path)) scheduleDecorate(path)
  scheduleDomain()
  scheduleAutosave()
}

// 树操作落地后的联动（与内容变化同款，只是不重复调度装饰）
function onFsMutated() {
  stashed.value = false
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

function applySchemas() {
  const meta = schemaMeta.value
  if (!meta) return
  const paths = fs.allFiles().filter((f) => !f.deleted).map((f) => f.path)
  const valuesSchema = fs.get('values.schema.json')?.content
  applyYamlSchemas(paths, meta, valuesSchema)
}

// 草稿恢复：整体重建 fs 与 models
function restoreDraft(payload: DraftPayload) {
  setSuppressAutosave(true)
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
  setSuppressAutosave(false)
  stashed.value = true // 恢复的内容本身来自暂存
}

async function onRestoreFromBanner() {
  const payload = draftInfo.value?.payload
  if (!payload) return
  restoreDraft(payload)
  ElMessage.success('草稿已恢复')
}

// ---- 校验 ----
// 失败返回 null（区别于「校验通过、无发现」的 []）：保存门禁据它中止——
// 此前失败被吞成 []，网络错误/500/越权都会被当成「校验通过」直接放行保存
async function doValidate(): Promise<ProblemItem[] | null> {
  const version = currentChartVersion()
  validating.value = true
  try {
    const body = fs.toSaveBody(version)
    const ps = await runValidate(body, isCreate.value ? 0 : props.appId)
    problems.value = ps
    validated.value = true
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

// 工具栏「校验」入口：校验本体（doValidate）同时被保存门禁复用，那边
// 自带错误提示——口头反馈只挂在按钮上，避免一次保存弹两轮 toast。
// 此前校验通过零反馈，用户无法区分「跑过了且干净」与「根本没跑」。
async function doValidateReport() {
  const ps = await doValidate()
  if (ps === null) return // 请求失败：doValidate 已报错
  if (ps.length) ElMessage.warning(`校验发现 ${ps.length} 个问题，见下方「问题」面板`)
  else ElMessage.success('校验通过，未发现问题')
}

// ---- 光标联动文档面板 ----
function onCursorChange(e: Monaco.editor.ICursorPositionChangedEvent) {
  cursorPos.value = { line: e.position.lineNumber, col: e.position.column }
  const monaco = monacoRef.value
  const m = getEditor()?.getModel()
  if (!monaco || !m) return
  // 逐行读取（getLineContent）而非 getValue()+split：光标每次移动都触发，
  // 512KB 上限的文件上每键 O(n) 字符串分配会造成可感卡顿；扫描窗口至多
  // 60 行，行级读取是 O(60)
  const line = m.getLineContent(e.position.lineNumber) || ''
  // 从当前行向上找最近的模块键（同任务块内）
  const ctl = controlKeys(schemaMeta.value)
  const pk = playKeysSet(schemaMeta.value)
  const keyRe = /^(\s*)(-\s+)?([\w.][\w-]*)\s*:/
  let found: string | null = null
  const indent = line.match(/^ */)![0].length
  for (let i = e.position.lineNumber; i >= 1 && i >= e.position.lineNumber - 59; i--) {
    const l = m.getLineContent(i) || ''
    const mm = l.match(keyRe)
    if (!mm) continue
    const li = mm[1].length + (mm[2] ? mm[2].length : 0)
    if (i === e.position.lineNumber || li <= indent) {
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

    // 主机名单独 catch：值补全的候选拿不到不阻断编辑器加载（组名复用
    // 同一批的 gs，不重复请求 /api/groups）
    const [schema, mods, ps, gs, hosts] = await Promise.all([
      fetchSchema(),
      api<ModuleMeta[]>('GET', '/api/modules'),
      api<Pool[]>('GET', '/api/pools'),
      api<GroupEntry[]>('GET', '/api/groups'),
      api<{ Name: string }[]>('GET', '/api/hosts').catch(() => null),
    ])
    if (disposed) return
    schemaMeta.value = schema
    modulesMeta.value = mods
    pools.value = ps
    groups.value = gs
    hostGroups.value = hosts
      ? { hosts: hosts.map((h) => h.Name), groups: gs.map((g) => g.Name) }
      : null

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
    const ed = monaco.editor.create(editorRef.value!, {
      ...editorOptions(),
      glyphMargin: true,
    })
    setEditor(ed)
    bindSuggestKey(monaco, ed)
    ed.onDidChangeCursorPosition(onCursorChange)
    ed.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, openSave)
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

onBeforeUnmount(() => {
  disposed = true
  document.removeEventListener('keydown', onDocKeydown)
  window.removeEventListener('beforeunload', onBeforeUnload)
  disposeDraft() // 自动暂存定时器
  clearTimeout(domainTimer)
  clearTimeout(schemaTimer)
  clearDecorTimers() // 装饰定时器：残留在已 dispose 的 model 上跑 applyDecorations 会抛「Model is disposed」
  providers?.dispose()
  providers = null
  disposeTabs() // editor 与全部 model
})

// 状态栏
const cursorPos = ref({ line: 1, col: 1 })
watch(active, () => { cursorPos.value = { line: 1, col: 1 } })

function jumpTo(path: string, line?: number) {
  openTab(path)
  if (line && getEditor()) {
    nextTick(() => getEditor()?.revealLineInCenter(line))
    nextTick(() => getEditor()?.setPosition({ lineNumber: line, column: 1 }))
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
      <!-- 新建模式整个应用都未入库，逐文件计数没有意义（脚手架 3 个文件
           一进来就"未保存"徒增困惑）；编辑模式才数改动文件 -->
      <span v-if="isCreate" class="dirty">● 新建未保存</span>
      <span v-else-if="dirtyCount" class="dirty">● {{ dirtyCount }} 处未保存</span>
      <span v-else class="saved muted">内容与底本一致</span>
      <div style="flex: 1" />
      <span class="muted draft-state">{{ draftStatus }}</span>
      <el-tooltip content="撤销上一步文件操作（Ctrl+Z 焦点不在编辑器时）" placement="bottom">
        <el-button :icon="RefreshLeft" :disabled="!fileOps.length" @click="undoFileOp" />
      </el-tooltip>
      <el-button @click="manualDraft">暂存</el-button>
      <el-button :loading="validating" @click="doValidateReport">校验</el-button>
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
            :validated="validated"
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
