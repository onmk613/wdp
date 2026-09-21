<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Coin, Collection, Plus, Position, PriceTag, Refresh, Search, WarningFilled,
} from '@element-plus/icons-vue'
import {
  api, type BatchResponse, type GroupEntry, type Host, type HostAlert, type LabelDef, type Pool, type ProbeResult,
} from '../api'
import {
  downloadText, hostCSVTemplate, parseHostCSV, parseSSHCSV, readCSVFile,
  sshCSVTemplate, type SSHCSVRow,
} from '../csv'
import LabelRows from '../components/LabelRows.vue'
import { can } from '../auth'

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

async function loadHosts(silent = false) {
  if (!silent) loadingHosts.value = true
  const seq = ++hostsReqSeq
  try {
    const q = search.value.trim() ? `?q=${encodeURIComponent(search.value.trim())}` : ''
    const [hs, alerts] = await Promise.all([
      api<Host[]>('GET', `/api/hosts${q}`),
      api<HostAlert[]>('GET', '/api/alerts').then((list) => {
        const m: Record<number, HostAlert[]> = {}
        for (const a of list) (m[a.HostID] ||= []).push(a)
        return m
      }).catch(() => ({})),
    ])
    if (seq !== hostsReqSeq) return // 已有更新的请求，丢弃本次旧响应
    hosts.value = hs
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
  if (list.some((a) => a.Level === 'crit')) return 'crit'
  if (list.length) return 'warn'
  return ''
}

function gotoDetail(h: Host) {
  void router.push(`/hosts/${h.ID}`)
}

// ---- agent 远程升级 ----
const upgrading = ref(false)

async function upgradeHost(row: Host) {
  try {
    await ElMessageBox.confirm(
      `升级主机 ${row.Name} 的 agent？将推送当前 server 配套的二进制并重启 agent 服务（执行中的任务会中断，耗时约半分钟）。`,
      '升级 agent', { type: 'warning', confirmButtonText: '升级', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  upgrading.value = true
  try {
    const r = await api<{ ok: boolean; from: string; to: string; detail: string }>(
      'POST', `/api/hosts/${row.ID}/upgrade`, {},
    )
    if (r.ok) ElMessage.success(`${row.Name}：${r.detail}`)
    else ElMessage.error(`${row.Name}：${r.detail}`)
    loadHosts(true)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    upgrading.value = false
  }
}

async function batchUpgrade() {
  const n = selection.value.length
  try {
    await ElMessageBox.confirm(
      `升级选中的 ${n} 台主机的 agent？并发 3 台逐台推送二进制并重启（每台约半分钟，执行中的任务会中断）。`,
      '批量升级 agent', { type: 'warning', confirmButtonText: '升级', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  upgrading.value = true
  try {
    const r = await api<{ results: { name: string; ok: boolean; detail: string }[]; ok: number; failed: number }>(
      'POST', '/api/hosts/upgrade', { ids: selection.value.map((x) => x.ID) },
    )
    if (r.failed === 0) ElMessage.success(`全部升级完成（${r.ok} 台）`)
    else {
      const bad = r.results.filter((x) => !x.ok).map((x) => `${x.name}：${x.detail}`).join('\n')
      await ElMessageBox.alert(bad, `升级完成：成功 ${r.ok} / 失败 ${r.failed}`, { type: 'warning' })
    }
    clearSelection()
    loadHosts(true)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    upgrading.value = false
  }
}

function onSearch() {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => loadHosts(), 300)
}

function statusType(status: string): 'success' | 'danger' | 'info' {
  return status === 'online' ? 'success' : status === 'offline' ? 'danger' : 'info'
}

function parseLabels(labels: string): Record<string, string> {
  try {
    return JSON.parse(labels || '{}')
  } catch {
    return {}
  }
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
  const ids = selection.value.map((h) => h.ID)
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
  const ids = selection.value.map((h) => h.ID)
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

// ---- 批量设置（池/组/标签）----
const batchVisible = ref(false)
const batchLoading = ref(false)
const batchForm = reactive({ pools: [] as string[], groups: [] as string[], replace: false })
const batchLabels = ref<Record<string, string>>({})
const batchSetPools = ref(false)
const batchSetGroups = ref(false)

function openBatchAssign() {
  Object.assign(batchForm, { pools: [], groups: [], replace: false })
  batchLabels.value = {}
  batchSetPools.value = false
  batchSetGroups.value = false
  batchVisible.value = true
}

async function submitBatchAssign() {
  const body: Record<string, unknown> = {
    ids: selection.value.map((h) => h.ID),
    action: 'assign',
    labels: batchLabels.value,
    replace_labels: batchForm.replace,
  }
  // 勾选的项整体替换（多值，空=清除）；未勾选保持不变
  if (batchSetPools.value) {
    body.pools = batchForm.pools
    body.set_pools = true
  }
  if (batchSetGroups.value) {
    body.groups = batchForm.groups
    body.set_groups = true
  }
  batchLoading.value = true
  try {
    const r = await api<BatchResponse>('POST', '/api/hosts/batch', body)
    ElMessage.success(`已更新 ${r.ok} 台`)
    batchVisible.value = false
    clearSelection()
    loadHosts()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    batchLoading.value = false
  }
}

// ---- 单机操作 ----
async function probe(row: Host) {
  try {
    const r = await api<ProbeResult>('POST', `/api/hosts/${row.ID}/probe`, {})
    if (r.status === 'online') {
      ElMessage.success(
        `${r.hostname} ${r.goos}/${r.arch} v${r.version} · 证书到期 ${r.cert_not_after || '-'}`,
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
      `删除主机 ${row.Name}？其 agent 将被通知自清理退役（删除二进制、证书与 systemd 单元）；agent 不可达时仅从台账移除。`,
      '确认删除',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    const r = await api<{ warning?: string }>('DELETE', `/api/hosts/${row.ID}`)
    if (r.warning) ElMessage.warning(r.warning)
    else ElMessage.success(`已删除 ${row.Name}（agent 已退役）`)
    loadHosts()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---- 编辑主机（改地址/池/组/标签）----
const editVisible = ref(false)
const editLoading = ref(false)
const editID = ref(0)
const editForm = reactive({ Name: '', Address: '', AgentPort: 7602, Pools: [] as string[], Groups: [] as string[] })
const editLabels = ref<Record<string, string>>({})

function openEdit(row: Host) {
  editID.value = row.ID
  Object.assign(editForm, {
    Name: row.Name,
    Address: row.Address,
    AgentPort: row.AgentPort,
    Pools: row.Pools || [],
    Groups: row.Groups || [],
  })
  editLabels.value = parseLabels(row.Labels)
  editVisible.value = true
}

async function submitEdit() {
  editLoading.value = true
  try {
    await api('PUT', `/api/hosts/${editID.value}`, {
      Address: editForm.Address,
      AgentPort: editForm.AgentPort,
      Pools: editForm.Pools,
      Groups: editForm.Groups,
      Labels: JSON.stringify(editLabels.value || {}),
    })
    ElMessage.success(`已更新 ${editForm.Name}`)
    editVisible.value = false
    loadHosts()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    editLoading.value = false
  }
}

// ---- 手动添加（多行 / CSV 批量）----
const addVisible = ref(false)
const addLoading = ref(false)
const addText = ref('')
const addFileInput = ref()

const addRows = computed(() => parseHostCSV(addText.value))
const addValidN = computed(() => addRows.value.filter((r) => r.value).length)

function labelsText(labels: Record<string, string>): string {
  return Object.entries(labels).map(([k, v]) => (v ? `${k}=${v}` : k)).join('; ')
}

function openAdd() {
  addText.value = ''
  addVisible.value = true
}

function onAddFilePicked(e: Event) {
  const input = e.target as HTMLInputElement
  const f = input.files?.[0]
  if (f) readCSVFile(f).then((t) => (addText.value = t.trim()))
  input.value = ''
}

async function submitAdd() {
  const rows = addRows.value.filter((r) => r.value)
  if (!rows.length) {
    ElMessage.warning('没有可导入的主机行（先按模版格式填写）')
    return
  }
  addLoading.value = true
  try {
    const r = await api<BatchResponse>('POST', '/api/hosts/import', {
      hosts: rows.map(({ value }) => ({
        name: value!.name,
        address: value!.address,
        agent_port: value!.agentPort,
        pools: value!.pools,
        groups: value!.groups,
        labels: value!.labels,
      })),
    })
    addVisible.value = false
    loadHosts()
    loadRegistries()
    if (r.failed > 0) {
      const bad = r.results
        .filter((x) => !x.OK)
        .map((x) => `第 ${rows[(x.Row ?? 1) - 1]?.line ?? '?'} 行 ${x.Name}：${x.Detail}`)
        .join('\n')
      await ElMessageBox.alert(bad, `导入完成：成功 ${r.ok} 台，失败 ${r.failed} 台`, { type: 'warning' })
    } else {
      ElMessage.success(`已导入 ${r.ok} 台主机`)
    }
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    addLoading.value = false
  }
}

// ---- 新建池 / 组 / 标签（可划入已有主机）----
const poolVisible = ref(false)
const poolLoading = ref(false)
const poolForm = reactive({ Name: '', Note: '', HostIDs: [] as number[] })

function openPool() {
  Object.assign(poolForm, { Name: '', Note: '', HostIDs: [] })
  poolVisible.value = true
}

async function submitPool() {
  const name = poolForm.Name.trim()
  if (!name) {
    ElMessage.warning('池名必填')
    return
  }
  if (pools.value.some((p) => p.Name === name)) {
    ElMessage.warning(`池 "${name}" 已存在（主机池下拉里可见）；如需调整成员请编辑主机或批量设置`)
    return
  }
  poolLoading.value = true
  try {
    await api('POST', '/api/pools', {
      name: poolForm.Name.trim(),
      note: poolForm.Note,
      host_ids: poolForm.HostIDs,
    })
    ElMessage.success(`已建池 ${poolForm.Name}（${poolForm.HostIDs.length} 台主机划入）`)
    poolVisible.value = false
    loadHosts()
    loadRegistries()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    poolLoading.value = false
  }
}

const groupVisible = ref(false)
const groupLoading = ref(false)
const groupForm = reactive({ Name: '', Note: '', HostIDs: [] as number[] })

function openGroup() {
  Object.assign(groupForm, { Name: '', Note: '', HostIDs: [] })
  groupVisible.value = true
}

async function submitGroup() {
  const name = groupForm.Name.trim()
  if (!name) {
    ElMessage.warning('组名必填')
    return
  }
  if (groups.value.some((g) => g.Name === name)) {
    ElMessage.warning(`组 "${name}" 已存在（组下拉里可见）`)
    return
  }
  groupLoading.value = true
  try {
    await api('POST', '/api/groups', {
      name: groupForm.Name.trim(),
      note: groupForm.Note,
      host_ids: groupForm.HostIDs,
    })
    ElMessage.success(`已建组 ${groupForm.Name}（${groupForm.HostIDs.length} 台主机划入）`)
    groupVisible.value = false
    loadHosts()
    loadRegistries()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    groupLoading.value = false
  }
}

const labelVisible = ref(false)
const labelLoading = ref(false)
const labelForm = reactive({ Key: '', Value: '', Note: '', HostIDs: [] as number[] })

function openLabel() {
  Object.assign(labelForm, { Key: '', Value: '', Note: '', HostIDs: [] })
  labelVisible.value = true
}

async function submitLabel() {
  const key = labelForm.Key.trim()
  if (!key) {
    ElMessage.warning('标签键必填')
    return
  }
  if (labelDefs.value.some((l) => l.Key === key)) {
    ElMessage.warning(`标签 "${key}" 已存在（可在主机编辑里直接使用）`)
    return
  }
  labelLoading.value = true
  try {
    await api('POST', '/api/labels', {
      key: labelForm.Key.trim(),
      value: labelForm.Value,
      note: labelForm.Note,
      host_ids: labelForm.HostIDs,
    })
    ElMessage.success(`已建标签 ${labelForm.Key}（${labelForm.HostIDs.length} 台主机附加）`)
    labelVisible.value = false
    loadHosts()
    loadRegistries()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    labelLoading.value = false
  }
}

// ---- 主机纳管（token 拉取）----
const enrollHost = ref('')
const enrollLoading = ref(false)
const enrollResult = ref<{ token: string; expires_in: number; command: string } | null>(null)
const copied = ref(false)

async function generateEnroll() {
  enrollLoading.value = true
  enrollResult.value = null
  try {
    enrollResult.value = await api('POST', '/api/enroll-tokens', {
      host: enrollHost.value || undefined,
    })
  } catch (e) {
    ElMessage.error((e as Error).message)
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
    setTimeout(() => (copied.value = false), 1500)
  } catch {
    // 非 HTTPS / 剪贴板权限被拒时 clipboard API 会 reject：提示手动复制，
    // 命令在上方只读输入框里可选中
    ElMessage.warning('自动复制失败（需 HTTPS 或剪贴板权限）：请手动选中命令复制')
  }
}

// ---- SSH 安装（批量：共享凭据 + 多行主机清单）----
const sshVisible = ref(false)
const sshText = ref('')
const sshFileInput = ref()
// 共享连接设置：应用到清单里 user/password 留空的行（行内值优先）
const sshShared = reactive({
  user: '', password: '', key_path: '', key_passphrase: '',
  verify_host_key: false, agent_port: 7602,
})

interface SSHJob {
  line: number
  host: SSHCSVRow
  status: 'queued' | 'installing' | 'ok' | 'failed'
  detail?: string
}
const sshJobs = ref<SSHJob[]>([])
const sshRows = computed(() => parseSSHCSV(sshText.value))
const sshValidN = computed(() => sshRows.value.filter((r) => r.value).length)
const sshRunning = computed(() => sshJobs.value.some((j) => j.status === 'queued' || j.status === 'installing'))

function openSSHInstall() {
  Object.assign(sshShared, {
    user: '', password: '', key_path: '', key_passphrase: '',
    verify_host_key: false, agent_port: 7602,
  })
  sshText.value = ''
  sshJobs.value = []
  sshVisible.value = true
}

function onSSHFilePicked(e: Event) {
  const input = e.target as HTMLInputElement
  const f = input.files?.[0]
  if (f) readCSVFile(f).then((t) => (sshText.value = t.trim()))
  input.value = ''
}

// 安装进行中不允许关对话框（关页面=放弃进度，服务端安装仍在跑）
function beforeSSHClose(done: () => void) {
  if (sshRunning.value) {
    ElMessage.warning('安装进行中，请等待完成')
    return
  }
  done()
}

const sshFailedN = computed(() => sshJobs.value.filter((j) => j.status === 'failed').length)

// 并发 3 执行排队中的任务（每台含二进制上传耗时较长，3 并发平衡速度与
// 目标机/网络压力）；逐台回写状态，单台失败不影响其余。
async function runSSHJobs() {
  const jobs = sshJobs.value
  let cursor = 0
  async function worker() {
    for (;;) {
      while (cursor < jobs.length && jobs[cursor].status !== 'queued') cursor++
      if (cursor >= jobs.length) return
      const job = jobs[cursor++]
      job.status = 'installing'
      try {
        await api('POST', '/api/agents/install', {
          name: job.host.name,
          address: job.host.address,
          ssh_port: job.host.sshPort || 22,
          user: job.host.user || sshShared.user,
          password: job.host.password || sshShared.password,
          key_path: sshShared.key_path,
          key_passphrase: sshShared.key_passphrase,
          verify_host_key: sshShared.verify_host_key,
          agent_port: sshShared.agent_port,
        })
        job.status = 'ok'
      } catch (e) {
        job.status = 'failed'
        job.detail = (e as Error).message
      }
    }
  }
  const queued = jobs.filter((j) => j.status === 'queued').length
  await Promise.all(Array.from({ length: Math.min(3, queued) }, worker))
  const ok = jobs.filter((j) => j.status === 'ok').length
  if (ok === jobs.length) ElMessage.success(`SSH 安装完成：${ok} 台全部上线`)
  else ElMessage.warning(`SSH 安装完成：成功 ${ok} / ${jobs.length}（失败行见表，可重试）`)
  loadHosts()
}

async function submitSSHInstall() {
  const rows = sshRows.value.filter((r) => r.value)
  if (!rows.length) {
    ElMessage.warning('没有可安装的主机行（先按模版格式填写）')
    return
  }
  sshJobs.value = rows.map((r) => ({ line: r.line, host: r.value!, status: 'queued' as const }))
  await runSSHJobs()
}

// 仅重试失败行（SSH 安装幂等：证书复用、systemd restart 拉起新二进制）
async function retrySSHFailed() {
  for (const j of sshJobs.value) {
    if (j.status === 'failed') {
      j.status = 'queued'
      j.detail = undefined
    }
  }
  await runSSHJobs()
}

const poolNames = computed(() => pools.value.map((p) => p.Name))
const groupNames = computed(() => groups.value.map((g) => g.Name))
const labelKeys = computed(() => labelDefs.value.map((l) => l.Key))

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
      <el-button size="small" type="primary" plain :loading="upgrading" @click="batchUpgrade">升级 agent</el-button>
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
            row-key="ID"
            style="width: 100%"
            @selection-change="onSelectionChange"
          >
            <el-table-column type="selection" width="44" reserve-selection />
            <el-table-column label="状态" width="130">
              <template #default="{ row }">
                <el-tag :type="statusType(row.Status)" effect="light" round>{{ row.Status }}</el-tag>
                <el-popover v-if="hostAlertLevel(row.ID)" placement="right" :width="380" trigger="click">
                  <template #reference>
                    <el-icon
                      :size="17" style="vertical-align: -3px; cursor: pointer; margin-left: 4px"
                      :color="hostAlertLevel(row.ID) === 'crit' ? '#f56c6c' : '#e6a23c'"
                    ><WarningFilled /></el-icon>
                  </template>
                  <div v-for="a in alertsByHost[row.ID] || []" :key="a.Kind" class="alert-item">
                    <el-tag size="small" :type="a.Level === 'crit' ? 'danger' : 'warning'" round>
                      {{ a.Level === 'crit' ? '严重' : '警告' }}
                    </el-tag>
                    {{ a.Detail }}
                    <span class="muted">（{{ a.UpdatedAt }}）</span>
                  </div>
                  <div class="muted" style="margin-top: 6px">点击主机名进入详情页查看趋势</div>
                </el-popover>
              </template>
            </el-table-column>
            <el-table-column label="主机名" min-width="130">
              <template #default="{ row }">
                <el-link type="primary" :underline="false" style="font-weight: 600" @click="gotoDetail(row)">{{ row.Name }}</el-link>
              </template>
            </el-table-column>
            <el-table-column label="地址" min-width="160">
              <template #default="{ row }">{{ row.Address }}:{{ row.AgentPort }}</template>
            </el-table-column>
            <el-table-column label="池" min-width="130">
              <template #default="{ row }">
                <el-tag v-for="p in row.Pools || []" :key="p" type="warning" effect="plain" class="label-tag">{{ p }}</el-tag>
                <span v-if="!(row.Pools || []).length" class="muted">-</span>
              </template>
            </el-table-column>
            <el-table-column label="组" min-width="110">
              <template #default="{ row }">
                <el-tag v-for="g in row.Groups || []" :key="g" type="info" effect="plain" class="label-tag">{{ g }}</el-tag>
                <span v-if="!(row.Groups || []).length" class="muted">-</span>
              </template>
            </el-table-column>
            <el-table-column label="标签" min-width="180">
              <template #default="{ row }">
                <el-tag
                  v-for="(v, k) in parseLabels(row.Labels)"
                  :key="k"
                  size="small"
                  type="primary"
                  effect="plain"
                  class="label-tag"
                >
                  {{ v ? `${k}=${v}` : k }}
                </el-tag>
                <span v-if="!Object.keys(parseLabels(row.Labels)).length" class="muted">-</span>
              </template>
            </el-table-column>
            <el-table-column label="最近在线" min-width="170">
              <template #default="{ row }">
                <span class="muted">{{ row.LastSeenAt || '-' }}</span>
              </template>
            </el-table-column>
            <el-table-column label="操作" width="270" fixed="right">
              <template #default="{ row }">
                <el-button link type="primary" @click="gotoDetail(row)">详情</el-button>
                <el-button v-if="can('host:edit')" link type="primary" @click="openEdit(row)">编辑</el-button>
                <el-button link type="primary" @click="probe(row)">探活</el-button>
                <el-button v-if="can('host:upgrade')" link type="warning" :disabled="upgrading" @click="upgradeHost(row)">升级</el-button>
                <el-button v-if="can('host:delete')" link type="danger" @click="removeHost(row)">删除</el-button>
              </template>
            </el-table-column>
            <template #empty>
              <el-empty description="没有匹配的主机：用 SSH 安装 / 纳管命令添加 agent，或手动添加" />
            </template>
          </el-table>
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

    <!-- 手动添加（多行 / CSV 批量） -->
    <el-dialog v-model="addVisible" title="手动添加主机（多行 / CSV 批量）" width="760px">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom: 10px"
        title="每行一台：name,address,agent_port,pools,groups,labels；整行只写一个 IP 也可以；池/组/标签多值用分号分隔；name 与端口留空 = address / 7602" />
      <div class="csv-actions">
        <el-button size="small" @click="downloadText('wdp-hosts.csv', hostCSVTemplate)">下载 CSV 模版</el-button>
        <el-button size="small" @click="addFileInput?.click()">上传 CSV 文件</el-button>
        <span class="muted">粘贴或上传后可继续编辑，下方实时预览并校验</span>
        <input ref="addFileInput" type="file" accept=".csv,text/csv" hidden @change="onAddFilePicked" />
      </div>
      <el-input
        v-model="addText" type="textarea" :rows="7" spellcheck="false"
        placeholder="web1,192.168.1.11,7602,web;prod,api,env=prod;rack=a1&#10;web2,192.168.1.12&#10;db1,192.168.1.21,,,db,"
      />
      <el-table v-if="addRows.length" :data="addRows" size="small" border max-height="220" style="margin-top: 10px">
        <el-table-column prop="line" label="行" width="52" />
        <el-table-column label="主机名" min-width="110">
          <template #default="{ row }">{{ row.value?.name ?? '—' }}</template>
        </el-table-column>
        <el-table-column label="地址" min-width="120">
          <template #default="{ row }">{{ row.value?.address ?? '—' }}</template>
        </el-table-column>
        <el-table-column label="端口" width="64">
          <template #default="{ row }">{{ row.value?.agentPort ?? '—' }}</template>
        </el-table-column>
        <el-table-column label="池" min-width="110">
          <template #default="{ row }">{{ row.value?.pools.join('; ') || '—' }}</template>
        </el-table-column>
        <el-table-column label="组" min-width="90">
          <template #default="{ row }">{{ row.value?.groups.join('; ') || '—' }}</template>
        </el-table-column>
        <el-table-column label="标签" min-width="130">
          <template #default="{ row }">{{ row.value ? labelsText(row.value.labels) || '—' : '—' }}</template>
        </el-table-column>
        <el-table-column label="错误" min-width="200">
          <template #default="{ row }">
            <span v-if="row.error" class="csv-err">第 {{ row.line }} 行：{{ row.error }}</span>
          </template>
        </el-table-column>
      </el-table>
      <template #footer>
        <el-button @click="addVisible = false">取消</el-button>
        <el-button type="primary" :loading="addLoading" :disabled="!addValidN" @click="submitAdd">
          导入 {{ addValidN }} 台主机
        </el-button>
      </template>
    </el-dialog>

    <!-- 编辑主机 -->
    <el-dialog v-model="editVisible" :title="`编辑主机 ${editForm.Name}`" width="520px">
      <el-form label-width="90px">
        <el-form-item label="地址">
          <el-input v-model="editForm.Address" />
        </el-form-item>
        <el-form-item label="agent 端口">
          <el-input-number v-model="editForm.AgentPort" :min="1" :max="65535" />
        </el-form-item>
        <el-form-item label="池（多选）">
          <el-select v-model="editForm.Pools" multiple style="width: 100%">
            <el-option v-for="p in poolNames" :key="p" :label="p" :value="p" />
          </el-select>
        </el-form-item>
        <el-form-item label="组（多选）">
          <el-select v-model="editForm.Groups" multiple style="width: 100%">
            <el-option v-for="g in groupNames" :key="g" :label="g" :value="g" />
          </el-select>
        </el-form-item>
        <el-form-item label="标签">
          <LabelRows v-model="editLabels" :keys="labelKeys" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="editVisible = false">取消</el-button>
        <el-button type="primary" :loading="editLoading" @click="submitEdit">保存</el-button>
      </template>
    </el-dialog>

    <!-- 批量设置 -->
    <el-dialog v-model="batchVisible" title="批量设置池 / 组 / 标签" width="520px">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom: 16px"
        :title="`将应用于选中的 ${selection.length} 台主机；未改动的项保持不变`" />
      <el-form label-width="90px">
        <el-form-item label="设置池">
          <el-checkbox v-model="batchSetPools">修改池归属（整体替换，空 = 清除）</el-checkbox>
          <el-select v-model="batchForm.pools" multiple :disabled="!batchSetPools" placeholder="替换为这些池" style="width: 100%">
            <el-option v-for="p in poolNames" :key="p" :label="p" :value="p" />
          </el-select>
        </el-form-item>
        <el-form-item label="设置组">
          <el-checkbox v-model="batchSetGroups">修改组归属（整体替换，空 = 清除）</el-checkbox>
          <el-select v-model="batchForm.groups" multiple :disabled="!batchSetGroups" placeholder="替换为这些组" style="width: 100%">
            <el-option v-for="g in groupNames" :key="g" :label="g" :value="g" />
          </el-select>
        </el-form-item>
        <el-form-item label="标签">
          <LabelRows v-model="batchLabels" :keys="labelKeys" />
          <el-checkbox v-model="batchForm.replace" style="margin-top: 6px">替换全部标签（而非追加）</el-checkbox>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="batchVisible = false">取消</el-button>
        <el-button type="primary" :loading="batchLoading" @click="submitBatchAssign">应用</el-button>
      </template>
    </el-dialog>

    <!-- 新建池 -->
    <el-dialog v-model="poolVisible" title="新建主机池" width="520px">
      <el-form label-width="110px">
        <el-form-item label="池名" required>
          <el-input v-model="poolForm.Name" placeholder="如 prod / test" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="poolForm.Note" />
        </el-form-item>
        <el-form-item label="划入主机">
          <el-select v-model="poolForm.HostIDs" multiple placeholder="可选：选择已有主机划入该池" style="width: 100%">
            <el-option v-for="hst in hosts" :key="hst.ID" :label="`${hst.Name} (${hst.Address})`" :value="hst.ID" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="poolVisible = false">取消</el-button>
        <el-button type="primary" :loading="poolLoading" @click="submitPool">创建</el-button>
      </template>
    </el-dialog>

    <!-- 新建组 -->
    <el-dialog v-model="groupVisible" title="新建组" width="520px">
      <el-form label-width="110px">
        <el-form-item label="组名" required>
          <el-input v-model="groupForm.Name" placeholder="如 web / db" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="groupForm.Note" />
        </el-form-item>
        <el-form-item label="划入主机">
          <el-select v-model="groupForm.HostIDs" multiple placeholder="可选：选择已有主机加入该组" style="width: 100%">
            <el-option v-for="hst in hosts" :key="hst.ID" :label="`${hst.Name} (${hst.Address})`" :value="hst.ID" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="groupVisible = false">取消</el-button>
        <el-button type="primary" :loading="groupLoading" @click="submitGroup">创建</el-button>
      </template>
    </el-dialog>

    <!-- 新建标签 -->
    <el-dialog v-model="labelVisible" title="新建标签" width="520px">
      <el-form label-width="110px">
        <el-form-item label="标签键" required>
          <el-input v-model="labelForm.Key" placeholder="如 env / tier" />
        </el-form-item>
        <el-form-item label="默认值">
          <el-input v-model="labelForm.Value" placeholder="可选（附加主机时写入的值）" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="labelForm.Note" />
        </el-form-item>
        <el-form-item label="附加主机">
          <el-select v-model="labelForm.HostIDs" multiple placeholder="可选：为已有主机加上该标签" style="width: 100%">
            <el-option v-for="hst in hosts" :key="hst.ID" :label="`${hst.Name} (${hst.Address})`" :value="hst.ID" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="labelVisible = false">取消</el-button>
        <el-button type="primary" :loading="labelLoading" @click="submitLabel">创建</el-button>
      </template>
    </el-dialog>

    <!-- SSH 安装 agent -->
    <!-- SSH 安装 agent（批量：共享凭据 + 多行主机清单） -->
    <el-dialog v-model="sshVisible" title="SSH 安装 agent（批量）" width="800px" :before-close="beforeSSHClose" :close-on-click-modal="!sshRunning">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom: 10px"
        title="适用于 server 可达目标机、目标机无法回连 server 的网络：server 经 SSH 推装 agent 并注册 systemd。凭据仅本次使用，不落库。" />
      <el-form label-width="110px" :disabled="sshRunning">
        <el-form-item label="SSH 用户">
          <el-input v-model="sshShared.user" placeholder="root（清单行内 user 优先）" style="width: 220px" />
        </el-form-item>
        <el-form-item label="SSH 密码">
          <el-input v-model="sshShared.password" type="password" show-password placeholder="与私钥二选一（行内 password 优先）" style="width: 320px" />
        </el-form-item>
        <el-form-item label="私钥路径">
          <el-input v-model="sshShared.key_path" placeholder="server 本机私钥路径（如 ~/.ssh/id_ed25519）" style="width: 320px" />
        </el-form-item>
        <el-form-item label="私钥口令">
          <el-input v-model="sshShared.key_passphrase" type="password" show-password placeholder="可选" style="width: 320px" />
        </el-form-item>
        <el-form-item label="指纹校验">
          <el-switch v-model="sshShared.verify_host_key" />
          <span class="muted" style="margin-left: 8px">校验 known_hosts（生产建议开；需预先采集指纹）</span>
        </el-form-item>
        <el-form-item label="agent 端口">
          <el-input-number v-model="sshShared.agent_port" :min="1" :max="65535" />
        </el-form-item>
      </el-form>

      <div class="csv-actions">
        <el-button size="small" :disabled="sshRunning" @click="downloadText('wdp-ssh-hosts.csv', sshCSVTemplate)">下载 CSV 模版</el-button>
        <el-button size="small" :disabled="sshRunning" @click="sshFileInput?.click()">上传 CSV 文件</el-button>
        <span class="muted">每行一台：name,address,ssh_port,user,password（整行只写一个 IP 也可以；后三项留空 = 用上方共享设置）</span>
        <input ref="sshFileInput" type="file" accept=".csv,text/csv" hidden @change="onSSHFilePicked" />
      </div>
      <el-input
        v-model="sshText" type="textarea" :rows="6" spellcheck="false" :disabled="sshRunning"
        placeholder="web1,192.168.1.11,22,root,&#10;web2,192.168.1.12&#10;db1,192.168.1.21,,root,another-pass"
      />

      <!-- 开始前：解析预览；开始后：逐台安装进度 -->
      <el-table
        v-if="sshJobs.length" :data="sshJobs" size="small" border max-height="240" style="margin-top: 10px"
      >
        <el-table-column prop="line" label="行" width="52" />
        <el-table-column label="主机名" min-width="110">
          <template #default="{ row }">{{ row.host.name }}</template>
        </el-table-column>
        <el-table-column label="地址" min-width="130">
          <template #default="{ row }">{{ row.host.address }}</template>
        </el-table-column>
        <el-table-column label="SSH 端口" width="80">
          <template #default="{ row }">{{ row.host.sshPort || 22 }}</template>
        </el-table-column>
        <el-table-column label="用户" width="90">
          <template #default="{ row }">{{ row.host.user || sshShared.user || 'root' }}</template>
        </el-table-column>
        <el-table-column label="状态" width="96">
          <template #default="{ row }">
            <el-tag v-if="row.status === 'queued'" type="info">排队</el-tag>
            <el-tag v-else-if="row.status === 'installing'" type="warning">安装中…</el-tag>
            <el-tag v-else-if="row.status === 'ok'" type="success">已上线</el-tag>
            <el-tag v-else type="danger">失败</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="详情" min-width="240">
          <template #default="{ row }">
            <span v-if="row.status === 'failed'" class="csv-err">{{ row.detail }}</span>
            <span v-else-if="row.status === 'installing'" class="muted">连接 / 上传二进制 / systemd 装配（约 1 分钟）</span>
          </template>
        </el-table-column>
      </el-table>
      <el-table v-else-if="sshRows.length" :data="sshRows" size="small" border max-height="240" style="margin-top: 10px">
        <el-table-column prop="line" label="行" width="52" />
        <el-table-column label="主机名" min-width="110">
          <template #default="{ row }">{{ row.value?.name ?? '—' }}</template>
        </el-table-column>
        <el-table-column label="地址" min-width="130">
          <template #default="{ row }">{{ row.value?.address ?? '—' }}</template>
        </el-table-column>
        <el-table-column label="SSH 端口" width="80">
          <template #default="{ row }">{{ row.value?.sshPort || 22 }}</template>
        </el-table-column>
        <el-table-column label="用户" width="90">
          <template #default="{ row }">{{ row.value?.user || sshShared.user || 'root' }}</template>
        </el-table-column>
        <el-table-column label="密码" width="80">
          <template #default="{ row }">{{ row.value?.password ? '••••' : '共享' }}</template>
        </el-table-column>
        <el-table-column label="错误" min-width="240">
          <template #default="{ row }">
            <span v-if="row.error" class="csv-err">第 {{ row.line }} 行：{{ row.error }}</span>
          </template>
        </el-table-column>
      </el-table>
      <template #footer>
        <el-button :disabled="sshRunning" @click="sshVisible = false">关闭</el-button>
        <el-button v-if="sshFailedN && !sshRunning" type="warning" @click="retrySSHFailed">
          重试失败 {{ sshFailedN }} 台
        </el-button>
        <el-button v-if="!sshJobs.length" type="primary" :disabled="!sshValidN" @click="submitSSHInstall">
          安装 {{ sshValidN }} 台（含上传二进制，每台约一分钟）
        </el-button>
      </template>
    </el-dialog>

  </div>
</template>

<style scoped>
.alert-item {
  margin: 6px 0;
  line-height: 1.6;
  font-size: 13px;
}
.csv-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.csv-err {
  color: #f56c6c;
  font-size: 12px;
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
