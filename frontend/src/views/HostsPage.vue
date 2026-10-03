<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Coin, Collection, Plus, Position, PriceTag, Refresh, Search, WarningFilled,
} from '@element-plus/icons-vue'
import {
  api, listUpgradeRuns, upgradeAgentRun, upgradeAgentsBatch,
  type BatchResponse, type GroupEntry, type Host, type HostAlert, type LabelDef, type Pool, type ProbeResult, type Run, type UpgradeRunRef,
} from '../api'
import SSHInstallDialog from '../components/hosts/SSHInstallDialog.vue'
import PoolGroupLabelDialog from '../components/hosts/PoolGroupLabelDialog.vue'
import EditHostDialog from '../components/hosts/EditHostDialog.vue'
import BatchAssignDialog from '../components/hosts/BatchAssignDialog.vue'
import ManualAddDialog from '../components/hosts/ManualAddDialog.vue'
import { can, serverBuild, serverBuildUnversioned } from '../auth'
import { parseLabels, statusType, fmtTime, runStatus } from '../lib/format'
import type { PagedResp } from '../lib/usePaging'
import Pager from '../components/Pager.vue'

const route = useRoute()
const router = useRouter()

// ---- 台账 ----
const hosts = ref<Host[]>([])
const loadingHosts = ref(false)
const search = ref('')
let pollTimer: number | undefined
let searchTimer: number | undefined

const alertsByHost = ref<Record<number, HostAlert[]>>({})
// 请求序号守卫：30s 轮询与防抖搜索可能并发，旧响应（尤其慢的搜索请求）
// 后到时不能覆盖新响应
let hostsReqSeq = 0

// 分页状态：page/pageSize/total 与后端 pagedResp 信封对齐；搜索在全量
// 数据上过滤（后端语义），命中多时翻页查看
const page = ref(1)
const pageSize = ref(50)
const total = ref(0)

async function loadHosts(silent = false) {
  if (!silent) loadingHosts.value = true
  const seq = ++hostsReqSeq
  try {
    const params = new URLSearchParams({ page: String(page.value), page_size: String(pageSize.value) })
    if (search.value.trim()) params.set('q', search.value.trim())
    const [paged, alerts] = await Promise.all([
      api<PagedResp<Host>>('GET', `/api/hosts?${params}`),
      api<HostAlert[]>('GET', '/api/alerts').then((list) => {
        const m: Record<number, HostAlert[]> = {}
        for (const a of list) (m[a.host_id] ||= []).push(a)
        return m
      }).catch(() => ({})),
    ])
    if (seq !== hostsReqSeq) return // 已有更新的请求，丢弃本次旧响应
    hosts.value = paged.items
    total.value = paged.total
    // 越界钳制（末页全删后停留空页）：回收到实际末页
    if (paged.items.length === 0 && paged.total > 0 && page.value > 1) {
      page.value = Math.ceil(paged.total / pageSize.value)
      return loadHosts(silent)
    }
    alertsByHost.value = alerts
  } catch (e) {
    if (seq !== hostsReqSeq) return
    if (!silent) ElMessage.error((e as Error).message)
  } finally {
    if (seq === hostsReqSeq) loadingHosts.value = false
  }
}

function hostAlertLevel(id: number): 'crit' | 'warn' | '' {
  const list = alertsByHost.value[id] || []
  if (list.some((a) => a.level === 'crit')) return 'crit'
  if (list.length) return 'warn'
  return ''
}

function gotoDetail(h: Host) {
  void router.push(`/hosts/${h.id}`)
}

// 升级进度对话框 → 执行记录页（run 深链无路由参数，列表页按 kind 过滤
// 最快定位）
function gotoRuns() {
  void router.push('/runs')
}

// 升级门控：agent build 与 server build 一致 = 已是最新。旧版 agent
// 不上报 build（空串）视为可升级（正是升级要解决的）；server build
// 未知（未登录探测完成）不门控，保持可用。未注入构建信息的开发构建
// （裸 go build，serverBuildUnversioned）版本串不反映二进制内容——
// 相等不能证明同版本，按可升级放行（点击后由后端重推二进制兜底）
function upgradable(h: Host): boolean {
  if (!serverBuild.value || serverBuildUnversioned.value) return true
  return !h.agent_build || h.agent_build !== serverBuild.value
}

// ---- agent 远程升级（后台 run + 轮询）----
// 提交即返回 run_id（后端逐台建 kind=upgrade 的 run，并发 3 后台推进），
// 进度靠轮询 /api/runs?kind=upgrade 对齐——此前同步等响应，单台最长约
// 7 分钟、批量几十分钟，挂起请求被反代/浏览器掐断后进度无从恢复。
// 断连/刷新后本页状态丢失，但 run 都在「执行记录」里可回看
interface UpgradeTrack {
  runId: number
  hostId: number
  name: string
  target: string
  status: string
  summary: string
}
const upgradeTracks = ref<UpgradeTrack[]>([])
const upgradeVisible = ref(false)
let upgradeTimer: number | undefined

// 升级按钮占用中：任一跟踪中的 run 未到终态（闸门在后端也会 409 兜底）
const upgrading = computed(() => upgradeTracks.value.some((t) => !runTerminal(t.status)))

function runTerminal(s: string): boolean {
  return s === 'succeeded' || s === 'failed' || s === 'cancelled'
}

function trackUpgrades(refs: UpgradeRunRef[]) {
  upgradeTracks.value = refs.map((x) => ({
    runId: x.run_id, hostId: x.host_id, name: x.name,
    target: serverBuild.value || '-', status: 'queued', summary: '',
  }))
  upgradeVisible.value = true
  scheduleUpgradePoll(300)
}

function scheduleUpgradePoll(delay = 3000) {
  window.clearTimeout(upgradeTimer)
  upgradeTimer = window.setTimeout(pollUpgrades, delay)
}

async function pollUpgrades() {
  if (!upgradeTracks.value.some((t) => !runTerminal(t.status))) return finishUpgrades()
  try {
    // 一次列表请求覆盖全部在途 run（逐台详情请求随主机数线性放大）
    const runs = await listUpgradeRuns(100)
    const byId = new Map(runs.map((r) => [r.id, r]))
    for (const t of upgradeTracks.value) {
      if (runTerminal(t.status)) continue
      const r = byId.get(t.runId)
      if (r) {
        t.status = r.status
        t.summary = r.summary || ''
        if (r.version) t.target = r.version
      }
    }
    // 列表 limit 封顶 100：更大批次里不在响应中的 run 单条补拉，否则
    // 它们永远停在 queued、轮询也不收敛
    const missing = upgradeTracks.value.filter((t) => !runTerminal(t.status) && !byId.has(t.runId))
    for (const t of missing) {
      try {
        const d = await api<{ run: Run }>('GET', `/api/runs/${t.runId}`)
        t.status = d.run.status
        t.summary = d.run.summary || ''
        if (d.run.version) t.target = d.run.version
      } catch { /* 单条失败：下一轮再试 */ }
    }
  } catch { /* 列表请求失败（网络抖动）：保持现状等下一轮 */ }
  if (upgradeTracks.value.some((t) => !runTerminal(t.status))) scheduleUpgradePoll()
  else finishUpgrades()
}

function finishUpgrades() {
  window.clearTimeout(upgradeTimer)
  if (!upgradeTracks.value.length) return
  const ok = upgradeTracks.value.filter((t) => t.status === 'succeeded').length
  if (upgradeTracks.value.length === 1) {
    const t = upgradeTracks.value[0]
    if (ok) ElMessage.success(`${t.name}：${t.summary}`)
    else ElMessage.error(`${t.name}：${t.summary}`)
  } else {
    const bad = upgradeTracks.value.length - ok
    if (bad === 0) ElMessage.success(`全部升级完成（${ok} 台）`)
    else ElMessage.warning(`升级结束：成功 ${ok} / 失败或取消 ${bad}（明细见对话框与执行记录）`)
  }
  loadHosts(true) // 终态已即时回写 AgentBuild，刷新「已最新」门控
}

async function upgradeHost(row: Host) {
  try {
    await ElMessageBox.confirm(
      `升级主机 ${row.name} 的 agent？将推送当前 server 配套的二进制并重启 agent 服务（执行中的任务会中断）。提交后在后台执行，关闭页面不影响。`,
      '升级 agent', { type: 'warning', confirmButtonText: '升级', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    const r = await upgradeAgentRun(row.id)
    trackUpgrades([{ run_id: r.run_id, host_id: row.id, name: row.name }])
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

async function batchUpgrade() {
  const n = selection.value.length
  try {
    await ElMessageBox.confirm(
      `升级选中的 ${n} 台主机的 agent？每台一个后台任务，并发 3 台逐台推送二进制并重启（每台约半分钟，执行中的任务会中断）。提交后可随时关闭页面，进度在「执行记录」可回看。`,
      '批量升级 agent', { type: 'warning', confirmButtonText: '升级', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    const r = await upgradeAgentsBatch(selection.value.map((x) => x.id))
    clearSelection()
    trackUpgrades(r.hosts)
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

function onSearch() {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => { page.value = 1; loadHosts() }, 300)
}

// ---- 注册表（池/组/标签）----
const pools = ref<Pool[]>([])
const groups = ref<GroupEntry[]>([])
const labelDefs = ref<LabelDef[]>([])

async function loadRegistries() {
  try {
    ;[pools.value, groups.value, labelDefs.value] = await Promise.all([
      api<Pool[]>('GET', '/api/pools'),
      api<GroupEntry[]>('GET', '/api/groups'),
      api<LabelDef[]>('GET', '/api/labels'),
    ])
  } catch {
    /* 静默：注册表加载失败不阻断台账 */
  }
}

// ---- 勾选与批量 ----
const selection = ref<Host[]>([])
const tableRef = ref()

function onSelectionChange(rows: Host[]) {
  selection.value = rows
}

function clearSelection() {
  tableRef.value?.clearSelection()
}

function doBatchNotify(n: number, verb: string): (r: BatchResponse) => void {
  const wrap = ElMessage.info(`${verb} ${n} 台主机中…`)
  return (r: BatchResponse) => {
    wrap.close()
    if (r.failed === 0) ElMessage.success(`${verb}完成：${r.ok} 台成功`)
    else ElMessage.warning(`${verb}完成：${r.ok} 成功 / ${r.failed} 失败`)
  }
}

async function batchProbe() {
  const ids = selection.value.map((h) => h.id)
  const rec = doBatchNotify(ids.length, '探活')
  try {
    const r = await api<BatchResponse>('POST', '/api/hosts/batch', { ids, action: 'probe' })
    rec(r)
    loadHosts(true)
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

async function batchDelete() {
  const n = selection.value.length
  try {
    await ElMessageBox.confirm(
      `删除选中的 ${n} 台主机？每台的 agent 将被通知自清理退役（删除二进制、证书与 systemd 单元）；不可达的 agent 仅从台账移除。`,
      '批量删除',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  const ids = selection.value.map((h) => h.id)
  const rec = doBatchNotify(ids.length, '删除')
  try {
    const r = await api<BatchResponse>('POST', '/api/hosts/batch', { ids, action: 'delete' })
    rec(r)
    clearSelection()
    loadHosts()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---- 对话框（自本页拆出的组件，见 components/hosts/：批量设置/编辑主机/
// 手动添加/新建池·组·标签。表单状态与重置逻辑随组件走，这里只持有开关）----
const batchVisible = ref(false)
const editVisible = ref(false)
const editRow = ref<Host | null>(null)
const addVisible = ref(false)
const poolVisible = ref(false)
const groupVisible = ref(false)
const labelVisible = ref(false)

function openBatchAssign() {
  batchVisible.value = true
}

function openEdit(row: Host) {
  editRow.value = row
  editVisible.value = true
}

function openAdd() {
  addVisible.value = true
}

function openPool() {
  poolVisible.value = true
}

function openGroup() {
  groupVisible.value = true
}

function openLabel() {
  labelVisible.value = true
}

// 对话框落库后的回刷：新建池/组/标签与导入主机都会同时动台账和注册表
function reloadAll() {
  loadHosts()
  loadRegistries()
}

// ---- 单机操作 ----
async function probe(row: Host) {
  try {
    const r = await api<ProbeResult>('POST', `/api/hosts/${row.id}/probe`, {})
    if (r.status === 'online') {
      // 版本优先取 build（二进制发布版本，形如 1.2.3-abc123）；version 是
      // 协议版本（v1），不但信息量低还会跟手写的 v 前缀叠成 "vv1"
      const ver = r.build || r.version || '-'
      ElMessage.success(
        `${r.hostname} ${r.goos}/${r.arch} · ${ver} · 证书到期 ${r.cert_not_after || '-'}`,
      )
    } else {
      ElMessage.warning(`不可达：${r.error}`)
    }
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
  loadHosts(true)
}

async function removeHost(row: Host) {
  try {
    await ElMessageBox.confirm(
      `删除主机 ${row.name}？其 agent 将被通知自清理退役（删除二进制、证书与 systemd 单元）；agent 不可达时仅从台账移除。`,
      '确认删除',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    const r = await api<{ warning?: string }>('DELETE', `/api/hosts/${row.id}`)
    if (r.warning) ElMessage.warning(r.warning)
    else ElMessage.success(`已删除 ${row.name}（agent 已退役）`)
    loadHosts()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---- 主机纳管（token 拉取）----
const enrollHost = ref('')
const enrollLoading = ref(false)
const enrollResult = ref<{ token: string; expires_in: number; command: string } | null>(null)
const copied = ref(false)
// copied 复位定时器：与 pollTimer/searchTimer 同口径，卸载时清理（否则
// 离开页面后仍会触发一次 copied 置回）
let copiedTimer: number | undefined

// 后端安全错误保持英文原文（可 grep、有测试断言）；这里只做展示层翻译，
// 其余错误原样透出
function enrollErrMsg(msg: string): string {
  if (msg.startsWith('refusing to hand out an enroll command over plaintext HTTP')) {
    return '当前是明文 HTTP，服务端拒绝发放纳管命令（脚本将以 root 执行）：请启用 --tls-cert/--tls-key、在可信反代终止 TLS（--trust-proxy）、设置 --advertise https://…，或在可信网络显式加 --allow-plaintext-enroll'
  }
  return msg
}

async function generateEnroll() {
  enrollLoading.value = true
  enrollResult.value = null
  try {
    enrollResult.value = await api('POST', '/api/enroll-tokens', {
      host: enrollHost.value || undefined,
    })
  } catch (e) {
    ElMessage.error(enrollErrMsg((e as Error).message))
  } finally {
    enrollLoading.value = false
  }
}

async function copyCommand() {
  if (!enrollResult.value) return
  try {
    await navigator.clipboard.writeText(enrollResult.value.command)
    copied.value = true
    ElMessage.success('已复制到剪贴板')
    window.clearTimeout(copiedTimer)
    copiedTimer = window.setTimeout(() => (copied.value = false), 1500)
  } catch {
    // 非 HTTPS / 剪贴板权限被拒时 clipboard API 会 reject：提示手动复制，
    // 命令在上方只读输入框里可选中
    ElMessage.warning('自动复制失败（需 HTTPS 或剪贴板权限）：请手动选中命令复制')
  }
}

// ---- SSH 安装（批量：共享凭据 + 多行主机清单）----
// 对话框拆到 components/hosts/SSHInstallDialog.vue（本页此前 8 个对话框
// 挤一个文件）。组件经 v-if 挂载：关闭即卸载，凭据随组件销毁不残留。
const sshVisible = ref(false)

function openSSHInstall() {
  sshVisible.value = true
}

const poolNames = computed(() => pools.value.map((p) => p.name))
const groupNames = computed(() => groups.value.map((g) => g.name))
const labelKeys = computed(() => labelDefs.value.map((l) => l.key))

onMounted(() => {
  const q = route.query.search
  if (typeof q === 'string' && q) search.value = q // 深链定位（分类管理跳转）
  loadHosts()
  loadRegistries()
  pollTimer = window.setInterval(() => loadHosts(true), 30_000)
})
// 已在本页时 query 变化也要响应（分类管理再次点另一台主机）
watch(
  () => route.query.search,
  (q) => {
    const v = typeof q === 'string' ? q : ''
    if (v !== search.value) {
      search.value = v
      loadHosts()
    }
  },
)
onUnmounted(() => {
  window.clearInterval(pollTimer)
  window.clearTimeout(searchTimer)
  window.clearTimeout(copiedTimer)
  window.clearTimeout(upgradeTimer)
})
</script>

<template>
  <div>
        <!-- 搜索 + 工具栏 -->
        <div class="toolbar">
          <el-input
            v-model="search"
            placeholder="搜索：主机名 / IP / 池 / 组 / 标签"
            :prefix-icon="Search"
            clearable
            style="width: 280px"
            @input="onSearch"
            @clear="onSearch()"
          />
          <div class="toolbar-actions">
            <el-button v-if="can('registry:manage')" :icon="Coin" @click="openPool">新建池</el-button>
            <el-button v-if="can('registry:manage')" :icon="Collection" @click="openGroup">新建组</el-button>
            <el-button v-if="can('registry:manage')" :icon="PriceTag" @click="openLabel">新建标签</el-button>
            <el-divider direction="vertical" />
            <el-button :icon="Refresh" @click="loadHosts()">刷新</el-button>
            <el-button v-if="can('host:enroll')" type="primary" plain :icon="Position" @click="openSSHInstall">
              SSH 安装 agent
            </el-button>
            <el-button v-if="can('host:enroll')" type="primary" :icon="Plus" @click="openAdd">手动添加</el-button>
          </div>
        </div>

        <!-- 批量操作栏 -->
        <div v-if="selection.length" class="batch-bar">
          <span>已选 <b>{{ selection.length }}</b> 台</span>
          <el-button size="small" @click="batchProbe">批量探活</el-button>
      <el-button size="small" type="primary" plain :loading="upgrading"
        :disabled="selection.length > 0 && selection.every((h) => !upgradable(h))"
        @click="batchUpgrade">
        升级 agent{{ selection.filter((h) => upgradable(h)).length ? `（${selection.filter((h) => upgradable(h)).length} 台可升）` : '' }}
      </el-button>
          <el-button v-if="can('host:edit')" size="small" type="primary" plain @click="openBatchAssign">批量设置池/组/标签</el-button>
          <el-button size="small" type="danger" plain @click="batchDelete">批量删除（agent 退役）</el-button>
          <el-button size="small" link @click="clearSelection">取消选择</el-button>
        </div>

        <!-- 主机台账 -->
        <el-card shadow="never">
          <el-table
            ref="tableRef"
            :data="hosts"
            v-loading="loadingHosts"
            row-key="id"
            style="width: 100%"
            @selection-change="onSelectionChange"
          >
            <el-table-column type="selection" width="44" reserve-selection />
            <el-table-column label="状态" width="130">
              <template #default="{ row }">
                <el-tag :type="statusType(row.status)" effect="light" round>{{ row.status }}</el-tag>
                <el-popover v-if="hostAlertLevel(row.id)" placement="right" :width="380" trigger="click">
                  <template #reference>
                    <el-icon
                      :size="17" style="vertical-align: -3px; cursor: pointer; margin-left: 4px"
                      :color="hostAlertLevel(row.id) === 'crit' ? '#f56c6c' : '#e6a23c'"
                    ><WarningFilled /></el-icon>
                  </template>
                  <div v-for="a in alertsByHost[row.id] || []" :key="a.kind + '-' + a.updated_at" class="alert-item">
                    <el-tag size="small" :type="a.level === 'crit' ? 'danger' : 'warning'" round>
                      {{ a.level === 'crit' ? '严重' : '警告' }}
                    </el-tag>
                    {{ a.detail }}
                    <span class="muted">（{{ a.updated_at }}）</span>
                  </div>
                  <div class="muted" style="margin-top: 6px">点击主机名进入详情页查看趋势</div>
                </el-popover>
              </template>
            </el-table-column>
            <el-table-column label="主机名" min-width="130">
              <template #default="{ row }">
                <el-link type="primary" :underline="false" style="font-weight: 600" @click="gotoDetail(row)">{{ row.name }}</el-link>
              </template>
            </el-table-column>
            <el-table-column label="地址" min-width="160">
              <template #default="{ row }">{{ row.address }}:{{ row.agent_port }}</template>
            </el-table-column>
            <el-table-column label="池" min-width="130">
              <template #default="{ row }">
                <el-tag v-for="p in row.pools || []" :key="p" type="warning" effect="plain" class="label-tag">{{ p }}</el-tag>
                <span v-if="!(row.pools || []).length" class="muted">-</span>
              </template>
            </el-table-column>
            <el-table-column label="组" min-width="110">
              <template #default="{ row }">
                <el-tag v-for="g in row.groups || []" :key="g" type="info" effect="plain" class="label-tag">{{ g }}</el-tag>
                <span v-if="!(row.groups || []).length" class="muted">-</span>
              </template>
            </el-table-column>
            <el-table-column label="标签" min-width="180">
              <template #default="{ row }">
                <el-tag
                  v-for="(v, k) in parseLabels(row.labels)"
                  :key="k"
                  size="small"
                  type="primary"
                  effect="plain"
                  class="label-tag"
                >
                  {{ v ? `${k}=${v}` : k }}
                </el-tag>
                <span v-if="!Object.keys(parseLabels(row.labels)).length" class="muted">-</span>
              </template>
            </el-table-column>
            <el-table-column label="最近在线" min-width="170">
              <template #default="{ row }">
                <span class="muted">{{ fmtTime(row.last_seen_at) }}</span>
              </template>
            </el-table-column>
            <el-table-column label="操作" width="270" fixed="right">
              <template #default="{ row }">
                <el-button link type="primary" @click="gotoDetail(row)">详情</el-button>
                <el-button v-if="can('host:edit')" link type="primary" @click="openEdit(row)">编辑</el-button>
                <el-button link type="primary" @click="probe(row)">探活</el-button>
                <el-tooltip v-if="can('host:upgrade') && !upgradable(row)" content="已是最新版本" placement="top">
                  <span><el-button link type="info" :disabled="true">已最新</el-button></span>
                </el-tooltip>
                <el-button v-else-if="can('host:upgrade')" link type="warning" :disabled="upgrading" @click="upgradeHost(row)">升级</el-button>
                <el-button v-if="can('host:delete')" link type="danger" @click="removeHost(row)">删除</el-button>
              </template>
            </el-table-column>
            <template #empty>
              <el-empty description="没有匹配的主机：用 SSH 安装 / 纳管命令添加 agent，或手动添加" />
            </template>
          </el-table>
          <Pager :page="page" :page-size="pageSize" :total="total" @update:page="(v) => { page = v; loadHosts(true) }" @update:page-size="(v) => { pageSize = v; loadHosts(true) }" />
        </el-card>

        <!-- 主机纳管（token 拉取） -->
        <el-card shadow="never" class="block">
          <template #header>
            <div class="card-header">
              <span>主机纳管（目标机可回连 server 时使用）</span>
              <span class="card-hint">生成一次性命令，在目标机以 root 执行</span>
            </div>
          </template>
          <el-form inline @submit.prevent="generateEnroll">
            <el-form-item label="预绑定主机名">
              <el-input v-model="enrollHost" placeholder="留空则以目标机 hostname 落账" style="width: 240px" clearable />
            </el-form-item>
            <el-form-item>
              <el-button v-if="can('host:enroll')" type="primary" :loading="enrollLoading" @click="generateEnroll">生成纳管命令</el-button>
            </el-form-item>
          </el-form>
          <div v-if="enrollResult" class="enroll-result">
            <el-input :model-value="enrollResult.command" readonly>
              <template #suffix>
                <el-button link type="primary" @click="copyCommand">{{ copied ? '已复制' : '复制' }}</el-button>
              </template>
            </el-input>
            <div class="enroll-meta">
              token {{ enrollResult.expires_in }} 分钟内单次有效 · 二进制取 server 同级 bin 目录 · 证书 SAN = 来源 IP + 主机名
            </div>
          </div>
        </el-card>

    <!-- agent 升级进度（后台 run 轮询）：关闭对话框只停止展示、不停止
         后台任务与轮询；断连/刷新后可从「执行记录」（kind=upgrade）恢复
         查看，逐步明细在 run 详情里 -->
    <el-dialog v-model="upgradeVisible" title="agent 升级进度" width="700px">
      <el-table :data="upgradeTracks" size="small" max-height="420">
        <el-table-column prop="name" label="主机" min-width="120" />
        <el-table-column prop="target" label="目标版本" min-width="130" />
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="runStatus(row.status)" round>{{ row.status }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="结果 / 摘要" min-width="220">
          <template #default="{ row }">
            <span class="muted">{{ row.summary || (runTerminal(row.status) ? '' : '升级中…') }}</span>
          </template>
        </el-table-column>
        <el-table-column label="run" width="80">
          <template #default="{ row }">
            <el-link type="primary" :underline="false" @click="gotoRuns">#{{ row.runId }}</el-link>
          </template>
        </el-table-column>
      </el-table>
      <template #footer>
        <span class="muted" style="margin-right: 12px">升级在服务端后台执行，关闭页面不影响；每台的逐步明细在「执行记录」详情里查看</span>
        <el-button @click="upgradeVisible = false">关闭</el-button>
      </template>
    </el-dialog>

    <!-- 手动添加（多行 / CSV 批量）：见 components/hosts/ManualAddDialog.vue -->
    <ManualAddDialog v-model="addVisible" @imported="reloadAll" />

    <!-- 编辑主机：见 components/hosts/EditHostDialog.vue -->
    <EditHostDialog
      v-model="editVisible"
      :host="editRow"
      :pool-names="poolNames"
      :group-names="groupNames"
      :label-keys="labelKeys"
      @saved="loadHosts()"
    />

    <!-- 批量设置：见 components/hosts/BatchAssignDialog.vue -->
    <BatchAssignDialog
      v-model="batchVisible"
      :ids="selection.map((h) => h.id)"
      :pool-names="poolNames"
      :group-names="groupNames"
      :label-keys="labelKeys"
      @applied="clearSelection(); loadHosts()"
    />

    <!-- 新建池 / 组 / 标签（同构表单参数化）：见 components/hosts/PoolGroupLabelDialog.vue -->
    <PoolGroupLabelDialog v-model="poolVisible" kind="pool" :hosts="hosts" :existing-names="poolNames" @created="reloadAll" />
    <PoolGroupLabelDialog v-model="groupVisible" kind="group" :hosts="hosts" :existing-names="groupNames" @created="reloadAll" />
    <PoolGroupLabelDialog v-model="labelVisible" kind="label" :hosts="hosts" :existing-names="labelKeys" @created="reloadAll" />

    <!-- SSH 安装 agent（批量）：见 components/hosts/SSHInstallDialog.vue -->
    <SSHInstallDialog v-if="sshVisible" v-model="sshVisible" @installed="loadHosts" />

  </div>
</template>

<style scoped>
.alert-item {
  margin: 6px 0;
  line-height: 1.6;
  font-size: 13px;
}
.muted {
  color: #909399;
  font-size: 12px;
}
.toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}
.toolbar-actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
}
.batch-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  background: #ecf5ff;
  border: 1px solid #d9ecff;
  border-radius: 6px;
  padding: 8px 14px;
  margin-bottom: 12px;
  color: #2563eb;
}
.block {
  margin-top: 16px;
}
.card-header {
  display: flex;
  align-items: center;
  gap: 12px;
  font-weight: 600;
}
.card-hint {
  font-size: 12px;
  font-weight: 400;
  color: #909399;
}
.label-tag {
  margin: 2px 4px 2px 0;
}
.enroll-result {
  margin-top: 4px;
}
.enroll-meta {
  font-size: 12px;
  color: #909399;
  margin-top: 8px;
}
</style>
