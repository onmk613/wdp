<script setup lang="ts">
// 裸 playbook 编辑器：单文件创作 + 下载到本地（CLI 执行：wdp run
// playbook.yaml -i inventory.yaml）。补全能力与 chart IDE 同源：play/任务
// 键、模块名与参数、chart 引用（map 形态，引用同级 charts/ 目录——
// 编辑器里无该目录上下文，name 无候选是预期行为）、{{ }} 内置变量。
// 校验：POST /api/playbook/validate（内容级静态检查，问题面板 + 行标记）。
// 保存为应用：play 的相位形态与 chart deploy.yaml 兼容，直接生成 chart
// 三件套入库。草稿存 localStorage（playbook 是本地文件，不进服务端库），
// 下载即完成——清草稿，重新进入是全新开始。
import { onBeforeUnmount, onMounted, reactive, ref, shallowRef } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Download, DocumentChecked, FolderAdd } from '@element-plus/icons-vue'
import type * as Monaco from 'monaco-editor'
import { api, createAppFromSpec, validatePlaybook, type ModuleMeta, type SchemaMeta } from '../api'
import { authUser } from '../auth'
import { ChartFS, scaffoldChart } from '../ide/fs'
import { loadMonaco, editorOptions, bindSuggestKey, modelURI, type MonacoNs } from '../ide/monaco'
import { applyYamlSchemas, fetchSchema } from '../ide/schemas'
import { controlKeys, playKeysSet } from '../ide/keys'
import { registerProviders } from '../ide/complete'
import { buildVarDomain, type VarDomain } from '../ide/vars'
import { applyDecorations } from '../ide/decorate'
import { parseIssueLine, applyMarkers, type ProblemItem } from '../ide/validate'

const router = useRouter()
// 草稿按用户隔离：playbook 内容常含内网主机名、账号与脚本逻辑，共享
// 浏览器上前一个用户的草稿不应弹给下一个用户（与 ide/draft.ts 同口径）
const draftKey = () => `wdp-playbook-draft-${authUser.value || 'anon'}`
const FILE = 'playbook.yaml'

const loading = ref(true)
const fs = new ChartFS()
const monacoRef = shallowRef<MonacoNs | null>(null)
const editorRef = ref<HTMLElement>()
let editor: Monaco.editor.IStandaloneCodeEditor | null = null
let model: Monaco.editor.ITextModel | null = null
// 补全 provider 的释放句柄：注册在 monaco 语言层全局生效，卸载必须
// dispose（不释放则路由往返叠加，候选出现多份）
let providers: Monaco.IDisposable | null = null
let decorIds: string[] = []

const schemaMeta = shallowRef<SchemaMeta | null>(null)
const modulesMeta = shallowRef<ModuleMeta[] | null>(null)
const domain = shallowRef<VarDomain | null>(null)
const hostGroups = shallowRef<{ hosts: string[]; groups: string[] } | null>(null)

const draftInfo = ref<{ content: string; saved_at: string } | null>(null)
const dirty = ref(false)
const draftStatus = ref('')

const scaffold = [
  '# playbook —— 下载后 CLI 执行：',
  '#   wdp run playbook.yaml -i inventory.yaml          # 在线模式',
  '#   wdp run playbook.yaml -i inventory.yaml --check   # 零风险预演',
  '# 引用 chart 用 map 形态（引用 playbook 同级 charts/ 目录）：',
  '#   - name: 部署 nginx',
  '#     chart:',
  '#       name: nginx',
  '- name: 第一个任务（示例，改掉我）',
  "  shell: 'echo hello'",
  '',
].join('\n')

function saveDraft(content: string) {
  try {
    localStorage.setItem(draftKey(), JSON.stringify({ content, saved_at: new Date().toISOString() }))
    draftStatus.value = `已暂存 ${new Date().toLocaleTimeString('zh-CN', { hour12: false })}`
  } catch {
    draftStatus.value = '暂存失败'
  }
}
let draftTimer = 0
function scheduleDraft() {
  clearTimeout(draftTimer)
  draftTimer = window.setTimeout(() => {
    if (dirty.value && model) saveDraft(model.getValue())
  }, 2500)
}

function restoreDraft(content: string) {
  if (model) model.setValue(content)
  draftInfo.value = null
  dirty.value = true
  ElMessage.success('草稿已恢复')
}
function discardDraft() {
  localStorage.removeItem(draftKey())
  draftInfo.value = null
}

function download() {
  if (!model) return
  const blob = new Blob([model.getValue()], { type: 'text/yaml' })
  const a = document.createElement('a')
  const url = URL.createObjectURL(blob)
  a.href = url
  a.download = FILE
  a.click()
  // 延迟 revoke：click 后同步 revoke 在部分浏览器（Safari）会中断尚未
  // 开始的下载（与 ide/csv.ts 的 downloadText 同口径）
  setTimeout(() => URL.revokeObjectURL(url), 1000)
  // 下载即完成：清掉草稿，从其他页面重新进入是全新开始（而不是又弹出
  // "检测到未恢复的草稿"）。之后继续编辑会重新产生草稿，互不影响
  localStorage.removeItem(draftKey())
  draftInfo.value = null
  draftStatus.value = `已下载 ${new Date().toLocaleTimeString('zh-CN', { hour12: false })}`
  ElMessage.success('已下载 playbook.yaml（本地草稿已清空）')
}

// ---- 校验（内容级静态检查：模块名/block 结构/模板 parse-only） ----
const problems = ref<ProblemItem[] | null>(null) // null = 尚未运行过
async function runCheck() {
  if (!model || !monacoRef.value) return
  try {
    const issues = await validatePlaybook(model.getValue())
    problems.value = issues.map((is) => ({ ...is, line: parseIssueLine(is.msg) }))
    applyMarkers(monacoRef.value, new Map([[FILE, model]]), problems.value)
    if (!problems.value.length) ElMessage.success('校验通过，未发现问题')
  } catch (e) {
    ElMessage.error(`校验失败：${(e as Error).message}`)
  }
}

// ---- 保存为应用（play 形态即部署相位，生成 chart 三件套入库） ----
const saveDlg = reactive({ visible: false, name: '', version: '1.0.0', desc: '', busy: false })
function openSaveAsApp() {
  if (!model) return
  saveDlg.name = ''
  saveDlg.version = '1.0.0'
  saveDlg.desc = ''
  saveDlg.busy = false
  saveDlg.visible = true
}
async function doSaveAsApp() {
  if (!model || saveDlg.busy) return
  saveDlg.busy = true
  try {
    await createAppFromSpec({
      name: saveDlg.name,
      version: saveDlg.version,
      description: saveDlg.desc,
      files: [
        {
          path: 'chart.yaml',
          content: scaffoldChart(saveDlg.name, saveDlg.version, saveDlg.desc),
          size: 0,
        },
        {
          path: 'values.yaml',
          content: '{}\n',
          size: 0,
        },
        {
          path: 'deploy.yaml',
          content: model.getValue(),
          size: 0,
        },
      ],
      delete_files: [],
      base_version: '',
      pools: [],
      groups: [],
      labels: '{}',
    })
    saveDlg.visible = false
    // 已毕业成应用：清草稿，避免下次进入又提示恢复
    localStorage.removeItem(draftKey())
    draftInfo.value = null
    dirty.value = false
    try {
      await ElMessageBox.confirm(
        `已保存为应用 ${saveDlg.name} ${saveDlg.version}（playbook 内容作为 deploy 相位）。打开应用列表查看？`,
        '保存成功',
        { confirmButtonText: '打开应用列表', cancelButtonText: '留在本页', type: 'success' },
      )
      router.push('/apps')
    } catch { /* 留在本页 */ }
  } catch (e) {
    ElMessage.error(`保存失败：${(e as Error).message}`)
  } finally {
    saveDlg.busy = false
  }
}

function rebuildDomain() {
  if (!schemaMeta.value || !model) return
  domain.value = buildVarDomain(new Map([[FILE, model.getValue()]]), schemaMeta.value)
}
function scheduleDecorate() {
  if (!monacoRef.value || !schemaMeta.value || !model) return
  const monaco = monacoRef.value
  clearTimeout(decorTimer)
  decorTimer = window.setTimeout(() => {
    if (!model) return
    const keys = { controlKeys: controlKeys(schemaMeta.value), playKeys: playKeysSet(schemaMeta.value) }
    decorIds = applyDecorations(monaco, model, decorIds, keys)
  }, 350)
}
let decorTimer = 0

// disposed：onMounted 是长异步链（monaco + schema/modules/hosts 请求），
// 快速进出路由时 await 之后仍会继续执行——若已卸载还创建 editor/models/
// providers，清理钩子早已跑过，资源净泄漏；且 model 占用的固定 URI
// 会让下次进入时 createModel 抛 "already exists"，编辑器直到刷新才恢复。
// 每个 await 后检查（与 AppIDE 同口径）。
let disposed = false

onMounted(async () => {
  try {
    const monaco = await loadMonaco()
    if (disposed) return
    monacoRef.value = monaco
    const [schema, mods] = await Promise.all([
      fetchSchema(),
      api<ModuleMeta[]>('GET', '/api/modules'),
    ])
    if (disposed) return
    schemaMeta.value = schema
    modulesMeta.value = mods
    try {
      const [hosts, hgroups] = await Promise.all([
        api<{ Name: string }[]>('GET', '/api/hosts'),
        api<{ Name: string }[]>('GET', '/api/groups'),
      ])
      if (disposed) return
      hostGroups.value = { hosts: hosts.map((h) => h.Name), groups: hgroups.map((g) => g.Name) }
    } catch { if (!disposed) hostGroups.value = null }

    // 内容：草稿 > 脚手架
    const content = scaffold
    try {
      const raw = localStorage.getItem(draftKey())
      if (raw) {
        const d = JSON.parse(raw)
        if (d && typeof d.content === 'string' && d.content.trim()) {
          draftInfo.value = { content: d.content, saved_at: d.saved_at }
        }
      }
    } catch { /* 损坏草稿忽略 */ }

    fs.create(FILE, content)
    model = monaco.editor.createModel(content, 'yaml', modelURI(FILE))
    syncLineCount()
    editor = monaco.editor.create(editorRef.value!, editorOptions())
    bindSuggestKey(monaco, editor)
    editor.setModel(model)
    model.onDidChangeContent(() => {
      dirty.value = true
      syncLineCount()
      rebuildDomain()
      scheduleDecorate()
      scheduleDraft()
    })
    providers = registerProviders(monaco, {
      fs,
      meta: () => schemaMeta.value,
      modules: () => modulesMeta.value,
      domain: () => domain.value,
      hostGroups: () => hostGroups.value,
      rootDefaultIsTask: () => false, // playbook 顶层是 play（相位文件才默认任务）
    })
    applyYamlSchemas([FILE], schemaMeta.value)
    rebuildDomain()
    scheduleDecorate()
    editor.focus()
  } catch (e) {
    if (!disposed) ElMessage.error(`加载失败：${(e as Error).message}`)
  } finally {
    if (!disposed) loading.value = false
  }
})

onBeforeUnmount(() => {
  disposed = true
  // 防抖定时器里的最后一段编辑：卸载时若仍有未落盘改动，先同步补存一次
  // （localStorage 写是同步的），否则最后 ≤2.5s 的编辑随定时器一起被清掉
  if (dirty.value && model) saveDraft(model.getValue())
  clearTimeout(draftTimer)
  clearTimeout(decorTimer)
  providers?.dispose()
  providers = null
  editor?.dispose()
  model?.dispose() // model dispose 同时解绑 onDidChangeContent，无需单独移除
})

// 状态栏行数：model 是普通 let，computed 追踪不到（求值一次后永不失效，
// 恒为 0）——改 ref 在 model 创建/切换与内容变更处手动同步
const lineCount = ref(0)
function syncLineCount() {
  lineCount.value = model ? model.getValue().split('\n').length : 0
}
</script>

<template>
  <div v-loading="loading" class="pb-root">
    <div class="pb-top">
      <b>Playbook 编辑器</b>
      <span class="muted">单文件 playbook · 下载后 CLI 执行（wdp run playbook.yaml -i inventory.yaml）</span>
      <span v-if="dirty" class="dirty">● 未保存</span>
      <div style="flex: 1" />
      <span class="muted">{{ draftStatus }}</span>
      <el-button @click="model && saveDraft(model.getValue())">暂存</el-button>
      <el-button :icon="DocumentChecked" @click="runCheck">校验</el-button>
      <el-button :icon="FolderAdd" @click="openSaveAsApp">保存为应用</el-button>
      <el-button type="primary" :icon="Download" @click="download">下载 playbook.yaml</el-button>
    </div>

    <el-alert v-if="draftInfo" type="warning" :closable="false" class="pb-banner">
      <template #title>
        检测到 {{ new Date(draftInfo.saved_at).toLocaleString() }} 的未恢复草稿
        <el-button size="small" type="primary" @click="restoreDraft(draftInfo.content)">恢复草稿</el-button>
        <el-button size="small" @click="discardDraft">丢弃</el-button>
      </template>
    </el-alert>

    <div v-if="problems && problems.length" class="pb-problems">
      <div class="ph">校验发现 {{ problems.length }} 个问题（修改后重新校验）</div>
      <div v-for="(p, i) in problems" :key="i" class="pitem" :class="{ err: p.level === 'ERROR' }">
        <span class="plv">{{ p.level }}</span>
        <span v-if="p.line" class="pline">L{{ p.line }}</span>
        <span class="pmsg">{{ p.msg }}</span>
      </div>
    </div>

    <el-dialog v-model="saveDlg.visible" title="保存为应用" width="480px" :close-on-click-modal="false">
      <el-form label-width="72px">
        <el-form-item label="应用名" required>
          <el-input v-model="saveDlg.name" placeholder="字母数字开头，可用 . _ -" />
        </el-form-item>
        <el-form-item label="版本">
          <el-input v-model="saveDlg.version" placeholder="默认 1.0.0" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="saveDlg.desc" placeholder="可选" />
        </el-form-item>
      </el-form>
      <div class="muted" style="font-size: 12px; padding: 0 12px">
        playbook 内容将作为 chart 的 deploy 相位入库（play 形态与相位兼容），
        values/chart 元数据自动生成，保存后可在应用列表与 IDE 中继续编辑。
      </div>
      <template #footer>
        <el-button @click="saveDlg.visible = false">取消</el-button>
        <el-button type="primary" :loading="saveDlg.busy" @click="doSaveAsApp">保存</el-button>
      </template>
    </el-dialog>

    <div ref="editorRef" class="pb-editor" />
    <div class="pb-status">
      <span class="muted">playbook.yaml</span>
      <span class="muted">{{ lineCount }} 行</span>
      <span class="grow" />
      <span class="muted">play/任务键 · 模块与参数 · chart 引用（map 形态）· {{ '{{' }} 变量 自动补全 · Alt+Space 手动触发</span>
    </div>
  </div>
</template>

<style scoped>
.pb-root {
  display: flex;
  flex-direction: column;
  height: calc(100vh - 116px);
  min-height: 480px;
  background: #fff;
  border: 1px solid #e4e7ed;
  border-radius: 8px;
  overflow: hidden;
}
.pb-top {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px 12px;
  border-bottom: 1px solid #e4e7ed;
  flex: none;
}
.dirty { color: #d97706; font-size: 12px; }
.pb-banner { border-radius: 0; flex: none; }
.pb-problems {
  flex: none;
  max-height: 160px;
  overflow: auto;
  border-bottom: 1px solid #fde2e2;
  background: #fffaf5;
  padding: 4px 12px;
  font-size: 12px;
}
.pb-problems .ph { color: #b45309; margin: 2px 0; }
.pitem { display: flex; gap: 8px; padding: 1px 0; color: #64748b; }
.pitem .plv { flex: none; width: 44px; color: #d97706; }
.pitem.err .plv { color: #dc2626; }
.pitem .pline { flex: none; color: #94a3b8; }
.pitem .pmsg { min-width: 0; }
.pb-editor { flex: 1; min-height: 200px; }
.pb-status {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 3px 12px;
  border-top: 1px solid #e4e7ed;
  background: #fafbfd;
  flex: none;
  font-size: 12px;
}
.grow { flex: 1; }
.muted { color: #909399; font-size: 12px; }
</style>
