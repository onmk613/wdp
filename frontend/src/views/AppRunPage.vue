<script setup lang="ts">
// 应用执行：左侧配置目标与执行清单，提交后**在下方内联展开执行输出**
// （SSE 事件驱动刷新，未收到事件前 2s 轮询兜底，任务随执行逐条出现）。
// 完整历史见「执行记录」菜单。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowDown, ArrowUp, CaretRight, Delete, InfoFilled, Refresh } from '@element-plus/icons-vue'
import { api, subscribeRuns, type App, type AppVersion, type Host, type Run, type RunTask } from '../api'
import { can } from '../auth'
import { runStatus, taskStatus } from '../lib/format'

const apps = ref<App[]>([])
// 执行目标按 run:execute 权限裁剪（资源显示与执行权限同口径）；
// 池/组/标签选项从目标主机派生，不再取全局注册表
const pools = ref<string[]>([])
const groups = ref<string[]>([])
const labels = ref<string[]>([])
const hosts = ref<Host[]>([])
const targetsRestricted = ref(false)

const selKind = ref('all')
const selValue = ref('')
const selHostIDs = ref<number[]>([])
// 自定义目标（kind=inline）：粘贴 inventory YAML，针对未纳管主机执行
//（conn: ssh 直连或自带证书材料的 agent）。作用域受限用户不可用
const inlineInv = ref('')
// 示例模板：真实可编辑文本（placeholder 半透明不可复制）——一键插入后
// 按实际环境改地址/凭据；密码建议用 password_env 引环境变量
const INLINE_EXAMPLE = `# conn: ssh 直连（密码走环境变量，避免明文落 chart/草稿）
webservers:
  hosts:
    node1: {host: 192.0.2.11, conn: ssh, user: root, password_env: NODE_PW}
    node2: {host: 192.0.2.12, conn: ssh, user: root, key_path: ~/.ssh/id_rsa}
# 已装 agent 的未纳管主机（自带证书材料）
agents:
  hosts:
    node3: {host: 192.0.2.13, conn: agent, agent_port: 7602}
`
function insertExample() {
  inlineInv.value = INLINE_EXAMPLE
}
function clearInline() {
  inlineInv.value = ''
}

// 行级相位：加入清单的每个条目独立选相位（默认 deploy），同一应用可
// 多次出现——组合执行中各条目真正执行的动作由各自的相位决定
type Item = { app_id: number; version: string; phase: string }
const items = ref<Item[]>([])
const pickAppId = ref<number | null>(null)

// 版本缓存（清单行内切换版本用）+ 各版本可用相位（行内相位下拉的数据源）
const versionCache = reactive<Record<number, string[]>>({})
const phasesCache = reactive<Record<string, string[]>>({})
// 迁移前旧版本未存相位清单：回退内置三相位（与改造前的全局下拉一致）
const LEGACY_PHASES = ['deploy', 'status', 'uninstall']

const running = ref(false)
const submitted = ref(false)
// 本次提交的 run 列表（内联输出区）
const activeRuns = ref<Run[]>([])
const activeTasks = reactive<Record<number, RunTask[]>>({})
let pollTimer: number | undefined

const labelOptions = computed(() => labels.value)
const appById = (id: number) => apps.value.find((a) => a.ID === id)

async function load(silent = false) {
  try {
    const [appsReq, targetsReq] = await Promise.all([
      api<App[]>('GET', '/api/apps'),
      api<{ hosts: Host[]; restricted: boolean }>('GET', '/api/exec/targets'),
    ])
    apps.value = appsReq
    hosts.value = targetsReq.hosts || []
    targetsRestricted.value = !!targetsReq.restricted
    deriveScopeOptions()
    // 版本清单按需加载（addItem 时）：挂载即对每个应用发 /api/apps/{id}
    // 是 N+1，应用多时首屏被拖慢
  } catch (e) {
    if (!silent) ElMessage.error((e as Error).message)
  }
}

// 从目标主机派生可选的池/组/标签（作用域用户看不到无关选项）
function deriveScopeOptions() {
  const ps = new Set<string>()
  const gs = new Set<string>()
  const ls = new Set<string>()
  for (const h of hosts.value) {
    for (const p of h.Pools || []) ps.add(p)
    for (const g of h.Groups || []) gs.add(g)
    try {
      for (const k of Object.keys(JSON.parse(h.Labels || '{}'))) ls.add(k)
    } catch {
      /* 忽略坏标签 */
    }
  }
  pools.value = [...ps]
  groups.value = [...gs]
  labels.value = [...ls]
}

async function loadVersions(appID: number) {
  if (versionCache[appID]) return
  try {
    const r = await api<{ versions: AppVersion[] }>('GET', `/api/apps/${appID}`)
    versionCache[appID] = r.versions.map((v) => v.Version)
    for (const v of r.versions) phasesCache[`${appID}:${v.Version}`] = v.Phases && v.Phases.length ? v.Phases : LEGACY_PHASES
  } catch {
    // 瞬时失败不落缓存：空数组也是 truthy，会让本会话对同一应用永不再重试
    delete versionCache[appID]
  }
}

// 行可选相位：该行所选版本的声明相位；未加载（新应用未返回）时回退内置
function phaseOptions(appID: number, version: string): string[] {
  return phasesCache[`${appID}:${version}`] || LEGACY_PHASES
}

// 版本切换后原相位在新版本不存在 → 重置 deploy（避免悬空相位提交被拒）
function onVersionChange(row: Item) {
  if (!phaseOptions(row.app_id, row.version).includes(row.phase)) row.phase = 'deploy'
}

// 加入执行清单：默认取该应用的默认版本（latest）、相位 deploy，加入后均可改
function addItem() {
  const app = appById(pickAppId.value || -1)
  if (!app) {
    ElMessage.warning('先选择应用')
    return
  }
  items.value.push({ app_id: app.ID, version: app.LatestVersion, phase: 'deploy' })
  loadVersions(app.ID)
  pickAppId.value = null
}

function move(i: number, dir: -1 | 1) {
  const j = i + dir
  if (j < 0 || j >= items.value.length) return
  const arr = items.value
  ;[arr[i], arr[j]] = [arr[j], arr[i]]
}

async function run() {
  if (!items.value.length) {
    ElMessage.warning('执行清单为空')
    return
  }
  const kind = selKind.value
  let label = '全部主机'
  if (kind === 'pool' || kind === 'group' || kind === 'label') {
    if (!selValue.value) {
      ElMessage.warning('请选择目标值')
      return
    }
    label = `${kind}=${selValue.value}`
  } else if (kind === 'hosts' && !selHostIDs.value.length) {
    ElMessage.warning('请勾选主机')
    return
  } else if (kind === 'inline') {
    if (!inlineInv.value.trim()) {
      ElMessage.warning('请粘贴 inventory 主机清单')
      return
    }
    label = '自定义目标（粘贴 inventory）'
  }
  try {
    // 逐条目摘要（app@版本[相位]，同应用多次出现时也一目了然）
    const parts = items.value.map((i) => `${appById(i.app_id)?.Name || i.app_id}@${i.version}[${i.phase}]`)
    const digest = parts.length > 6 ? parts.slice(0, 6).join(' → ') + ' → …' : parts.join(' → ')
    await ElMessageBox.confirm(
      `按顺序执行 ${items.value.length} 个条目到【${label}】？\n${digest}`,
      '应用执行', { type: 'warning', confirmButtonText: '执行', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  running.value = true
  try {
    const selector: Record<string, unknown> = { kind, value: selValue.value, host_ids: selHostIDs.value }
    if (kind === 'inline') selector.inventory = inlineInv.value
    const payload = { items: items.value, selector }
    const r = await api<{ run_ids: number[]; hosts: number }>('POST', '/api/runs', payload)
    lastSubmit.value = payload // 重新执行按原样重发（目标选择器随提交快照）
    ElMessage.success(`已提交：目标 ${r.hosts} 台主机，共 ${r.run_ids.length} 个应用（下方实时输出）`)
    submitted.value = true
    awaitingFirstPoll.value = true
    activeRuns.value = []
    trackedRunIDs = r.run_ids
    for (const k of Object.keys(activeTasks)) delete activeTasks[Number(k)]
    await pollActive(r.run_ids)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    running.value = false
  }
}

// 最近一次提交的原始载荷（重新执行用：目标与清单按当时快照重发）
const lastSubmit = ref<{ items: Item[]; selector: Record<string, unknown> } | null>(null)

// activeRunActive：queued/running/cancelling 都算未收尾
function runActive(r: Run): boolean {
  return r.Status === 'running' || r.Status === 'queued' || r.Status === 'cancelling'
}

// cancelRun 取消一个进行中的 run：在途任务会执行完，已执行结果保留；
// 幂等模块下重新发起即断点续跑
async function cancelRun(r: Run) {
  try {
    await ElMessageBox.confirm(
      `取消 ${r.AppName}@${r.Version}[${r.Phase || 'deploy'}] 的执行？在途任务会执行完，已执行任务的结果保留，可重新发起。`,
      '取消执行', { type: 'warning', confirmButtonText: '取消执行', cancelButtonText: '再想想' },
    )
  } catch {
    return
  }
  try {
    await api('POST', `/api/runs/${r.ID}/cancel`, {})
    ElMessage.success('已请求取消（在途任务执行完后生效）')
    pollSoon()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// rerunRun 重新执行：按该 run 的应用/版本/相位 + 最近一次提交的目标
// 选择器原样重发（幂等模块下等于断点续跑；改清单/目标后请用主执行按钮）
async function rerunRun(r: Run) {
  if (!lastSubmit.value) {
    ElMessage.warning('本会话没有该次提交的目标快照，请重新配置后执行')
    return
  }
  if (activeRuns.value.some(runActive)) {
    ElMessage.warning('仍有执行未收尾，先取消或等它完成')
    return
  }
  try {
    await ElMessageBox.confirm(
      `重新执行 ${r.AppName}@${r.Version}[${r.Phase || 'deploy'}]（目标按上次提交快照）？`,
      '重新执行', { type: 'warning', confirmButtonText: '执行', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  running.value = true
  try {
    const payload = {
      items: [{ app_id: r.AppID, version: r.Version, phase: r.Phase || 'deploy' }],
      selector: lastSubmit.value.selector,
    }
    const resp = await api<{ run_ids: number[]; hosts: number }>('POST', '/api/runs', payload)
    lastSubmit.value = payload
    submitted.value = true
    awaitingFirstPoll.value = true
    activeRuns.value = []
    trackedRunIDs = resp.run_ids
    for (const k of Object.keys(activeTasks)) delete activeTasks[Number(k)]
    await pollActive(resp.run_ids)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    running.value = false
  }
}

// pollActive 拉取本次提交的 run 与任务明细（逐条出现）。
// - in-flight 防重入：2s 定时器与 run() 的首轮拉取重叠时只跑一份；
// - 合并式更新：单个 run 请求瞬时失败（网络抖动/500）时保留上一轮的
//   条目——用成功子集整体替换会把该 run 从清单里挤掉，且后续轮询只认
//   activeRuns 里的 ID，丢掉的 run 即使还在 running 也永远不再拉取，
//   用户看到"已完成"却缺了一段输出；
// - 本轮全部请求失败（网络抖动）时保留旧状态且不算完成——否则首轮全败
//   会把 activeRuns 置空，anyRunning() 误判已完成、轮询停摆
let pollInFlight = false
// 提交后尚未成功拿到任何 run 状态：此期间视为进行中（防首轮全败误判）
const awaitingFirstPoll = ref(false)
// 本次提交的 run 全集（轮询固定拉全集，不随展示清单丢失）
let trackedRunIDs: number[] = []

async function pollActive(runIDs: number[]) {
  if (pollInFlight) return
  pollInFlight = true
  try {
    // 逐 run 并行拉取：串行 for-await 时 5 个应用就是 5 倍串行延迟，
    // 且每 2s 一轮持续放大
    const results = await Promise.allSettled(
      runIDs.map((id) => api<{ run: Run; tasks: RunTask[] }>('GET', `/api/runs/${id}`)),
    )
    const prev = new Map(activeRuns.value.map((r) => [r.ID, r]))
    const out: Run[] = []
    let ok = 0
    results.forEach((r, i) => {
      const id = runIDs[i]
      if (r.status !== 'fulfilled') {
        // 单个失败：沿用上一轮数据（若有），没有就只能缺席到下一轮
        const old = prev.get(id)
        if (old) out.push(old)
        return
      }
      out.push(r.value.run)
      activeTasks[id] = r.value.tasks || []
      ok++
    })
    if (ok === 0) return // 全败：不更新 activeRuns，也不解除 awaiting
    awaitingFirstPoll.value = false
    activeRuns.value = out
  } finally {
    pollInFlight = false
  }
}

function anyRunning(): boolean {
  if (awaitingFirstPoll.value) return true
  if (activeRuns.value.some(runActive)) return true
  // 有跟踪但从未成功拿到状态的 run（每轮都失败）：视为进行中继续轮询，
  // 否则其余 run 全部终态后轮询停摆，该 run 的输出与终态永远缺失
  return trackedRunIDs.some((id) => !activeRuns.value.some((r) => r.ID === id))
}

// 事件驱动为主：run 事件（状态变化/任务落库）触发立即拉取详情；
// 2s 定时轮询只在 SSE 尚未收到过事件时全速跑（兜底），收到过即放缓到
// 30s——提交后的实时输出不再依赖固定频率的全量轮询
let unsubRuns: (() => void) | undefined
let sseAlive = false

function pollSoon() {
  // 拉全集而非 activeRuns：展示清单某轮缺一个（瞬时失败）也不丢跟踪
  if (submitted.value && anyRunning()) {
    void pollActive(trackedRunIDs)
  }
}

function schedulePoll() {
  pollTimer = window.setTimeout(() => {
    pollSoon()
    schedulePoll()
  }, sseAlive ? 30000 : 2000)
}

onMounted(() => {
  load()
  unsubRuns = subscribeRuns(
    () => {
      sseAlive = true
      pollSoon()
    },
    () => {
      // 连接失败（server 重启/502）后 EventSource 不再自动重连：回落快档
      // 轮询，执行输出的实时性不受影响
      sseAlive = false
    },
  )
  schedulePoll()
})
onUnmounted(() => {
  window.clearTimeout(pollTimer)
  unsubRuns?.()
})

// 执行未收尾时拦截路由离开：结果只在「本次执行输出」区实时展示，误点
// 别处回来就只剩执行记录的静态明细——确认后再放行（执行本身不受影响）
onBeforeRouteLeave(async () => {
  if (!submitted.value || !anyRunning()) return true
  try {
    await ElMessageBox.confirm(
      '仍有应用在执行中，离开后这里不再实时刷新（可随时回「执行记录」查看结果）。',
      '执行进行中', { type: 'warning', confirmButtonText: '仍要离开', cancelButtonText: '留在本页' },
    )
    return true
  } catch {
    return false
  }
})
</script>

<template>
  <div>
    <div class="grid">
      <el-card shadow="never" class="block">
        <template #header><span>执行目标</span></template>
        <el-alert v-if="targetsRestricted" type="info" :closable="false" show-icon style="margin-bottom: 10px"
          title="目标主机已按你的执行权限（run:execute 作用域）裁剪——清单之外的主机不可执行。" />
        <el-form label-width="90px">
          <el-form-item label="范围">
            <el-radio-group v-model="selKind">
              <el-radio-button value="all">{{ targetsRestricted ? '全部可执行主机' : '全部主机' }}</el-radio-button>
              <el-radio-button value="pool">池</el-radio-button>
              <el-radio-button value="group">组</el-radio-button>
              <el-radio-button value="label">标签</el-radio-button>
              <el-radio-button value="hosts">手动选</el-radio-button>
              <el-radio-button value="inline" :disabled="targetsRestricted">自定义</el-radio-button>
            </el-radio-group>
          </el-form-item>
          <el-form-item v-if="selKind === 'pool'" label="池">
            <el-select v-model="selValue" placeholder="选择池" style="width: 100%">
              <el-option v-for="p in pools" :key="p" :label="p" :value="p" />
            </el-select>
          </el-form-item>
          <el-form-item v-else-if="selKind === 'group'" label="组">
            <el-select v-model="selValue" placeholder="选择组" style="width: 100%">
              <el-option v-for="g in groups" :key="g" :label="g" :value="g" />
            </el-select>
          </el-form-item>
          <el-form-item v-else-if="selKind === 'label'" label="标签">
            <el-select v-model="selValue" placeholder="选择标签键（含该键的主机）" style="width: 100%">
              <el-option v-for="l in labelOptions" :key="l" :label="l" :value="l" />
            </el-select>
          </el-form-item>
          <el-form-item v-else-if="selKind === 'hosts'" label="主机">
            <el-select v-model="selHostIDs" multiple filterable placeholder="勾选主机" style="width: 100%">
              <el-option v-for="h in hosts" :key="h.ID" :label="`${h.Name} (${h.Address})`" :value="h.ID" />
            </el-select>
          </el-form-item>
          <template v-else-if="selKind === 'inline'">
            <el-alert type="info" :closable="false" show-icon style="margin-bottom: 8px"
              title="粘贴 inventory YAML 执行未纳管主机（conn: ssh 直连或自带证书材料的 agent）。清单内容（可能含凭据）不落库，仅在执行期使用；需要全局 run:execute 权限。" />
            <el-form-item label="inventory">
              <div class="inv-tools">
                <el-button size="small" @click="insertExample">插入示例模板</el-button>
                <el-button size="small" :disabled="!inlineInv" @click="clearInline">清空</el-button>
                <span class="muted">示例可直接编辑；password_env 引用控制端环境变量，避免明文凭据</span>
              </div>
              <el-input v-model="inlineInv" type="textarea" :rows="8" spellcheck="false"
                placeholder="粘贴 inventory YAML，或点「插入示例模板」获取可编辑模板"
                style="font-family: ui-monospace, Menlo, monospace; width: 100%" />
            </el-form-item>
          </template>
        </el-form>
      </el-card>

      <el-card shadow="never" class="block">
        <template #header><span>执行清单（按顺序）</span></template>
        <div class="pick-row">
          <el-select v-model="pickAppId" placeholder="选择应用（加入后取默认版本，可再改）" filterable style="flex: 1">
            <el-option v-for="a in apps" :key="a.ID" :label="a.Name" :value="a.ID" />
          </el-select>
          <el-button type="primary" plain @click="addItem">加入</el-button>
        </div>
        <el-table :data="items" size="small" style="margin-top: 8px">
          <el-table-column label="#" type="index" width="44" />
          <el-table-column label="应用" min-width="120">
            <template #default="{ row }">{{ appById(row.app_id)?.Name || row.app_id }}</template>
          </el-table-column>
          <el-table-column label="版本" min-width="130">
            <template #default="{ row }">
              <el-select v-model="row.version" size="small" style="width: 100%" @change="onVersionChange(row)">
                <el-option v-for="v in versionCache[row.app_id] || [row.version]" :key="v" :label="v" :value="v" />
              </el-select>
            </template>
          </el-table-column>
          <el-table-column min-width="110">
            <template #header>
              相位
              <el-tooltip placement="top" content="每个条目独立选择（默认 deploy）：同一应用可在清单中出现多次、各执行不同相位，按行序组合出整套动作。选项来自该版本 chart 实际具备的相位；uninstall 需目标机有 release marker（执行过 deploy）">
                <el-icon style="vertical-align: -2px"><InfoFilled /></el-icon>
              </el-tooltip>
            </template>
            <template #default="{ row }">
              <el-select v-model="row.phase" size="small" style="width: 100%">
                <el-option v-for="p in phaseOptions(row.app_id, row.version)" :key="p" :label="p" :value="p" />
              </el-select>
            </template>
          </el-table-column>
          <el-table-column label="排序/移除" width="150">
            <template #default="{ $index }">
              <el-button link :icon="ArrowUp" @click="move($index, -1)" />
              <el-button link :icon="ArrowDown" @click="move($index, 1)" />
              <el-button link type="danger" :icon="Delete" @click="items.splice($index, 1)" />
            </template>
          </el-table-column>
          <template #empty><el-empty description="执行清单为空：选择应用后点「加入」" /></template>
        </el-table>
        <div class="run-row">
          <el-button :icon="Refresh" @click="load()">刷新</el-button>
          <div style="flex: 1" />
          <el-tooltip v-if="submitted && anyRunning()" placement="top"
            :content="`正在执行：${activeRuns.filter(runActive).map((r) => `${r.AppName}@${r.Version}`).join('、') || '提交中…'}——完成或取消后再发起新执行`">
            <span>
              <el-button type="primary" :icon="CaretRight" disabled>执行</el-button>
            </span>
          </el-tooltip>
          <el-button v-else-if="can('run:execute')" type="primary" :icon="CaretRight" :loading="running" :disabled="!items.length" @click="run">
            执行
          </el-button>
        </div>
      </el-card>
    </div>

    <!-- 本次执行输出（内联展开，实时刷新（SSE）+ 轮询兜底） -->
    <el-card v-if="submitted" shadow="never" class="block">
      <template #header>
        <div class="out-head">
          <span>本次执行输出</span>
          <span class="muted">
            {{ anyRunning()
              ? `执行中：${activeRuns.filter(runActive).map((r) => `${r.AppName}@${r.Version}`).join('、') || '提交中…'}（实时刷新，可取消）`
              : '已完成' }} · 完整历史见左侧菜单「执行记录」
          </span>
        </div>
      </template>
      <div v-for="r in activeRuns" :key="r.ID" class="run-block" :class="{ 'run-active': runActive(r) }">
        <div class="run-title">
          <el-tag :type="runStatus(r.Status)" round size="small">{{ r.Status }}</el-tag>
          <b style="margin-left: 8px">{{ r.AppName }}@{{ r.Version }}</b>
          <el-tag size="small" type="info" effect="plain" style="margin-left: 6px">{{ r.Phase || 'deploy' }}</el-tag>
          <span class="muted" style="margin-left: 8px">run #{{ r.ID }} · {{ r.Summary }}</span>
          <span class="muted" style="margin-left: 8px">已执行 {{ (activeTasks[r.ID] || []).length }} 条任务</span>
          <div style="flex: 1" />
          <el-button v-if="runActive(r) && r.Status !== 'cancelling' && can('run:execute')" size="small" type="warning" plain @click="cancelRun(r)">
            取消执行
          </el-button>
          <el-button v-else-if="!runActive(r) && can('run:execute')" size="small" plain @click="rerunRun(r)">
            重新执行
          </el-button>
        </div>
        <el-table :data="activeTasks[r.ID] || []" size="small">
          <el-table-column prop="Task" label="任务" min-width="160" />
          <el-table-column prop="Module" label="模块" width="90" />
          <el-table-column prop="Host" label="主机" min-width="110" />
          <el-table-column label="状态" width="120">
            <template #default="{ row }">
              <el-tag size="small" :type="taskStatus(row.Status)">
                {{ row.Status }}{{ row.Changed ? '·changed' : '' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="输出" min-width="260">
            <template #default="{ row }">
              <pre class="out">{{ (row.Detail || '(无输出)').slice(0, 2000) }}</pre>
            </template>
          </el-table-column>
        </el-table>
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}
@media (max-width: 1100px) {
  .grid {
    grid-template-columns: 1fr;
  }
}
.block {
  margin-bottom: 16px;
}
.pick-row {
  display: flex;
  gap: 8px;
}
.inv-tools {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
  width: 100%;
}
.run-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 12px;
}
.out-head {
  display: flex;
  align-items: center;
  gap: 12px;
  font-weight: 600;
}
.run-block {
  margin-bottom: 14px;
  padding: 8px 10px;
  border-radius: 6px;
}
.run-active {
  background: #f0f7ff;
  border: 1px solid #d4e7fc;
}
.run-title {
  display: flex;
  align-items: center;
  margin-bottom: 6px;
}
.out {
  background: #0f172a;
  color: #e2e8f0;
  border-radius: 6px;
  padding: 6px 8px;
  font-size: 12px;
  margin: 0;
  max-height: 160px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.muted {
  color: #909399;
  font-size: 12px;
}
</style>
