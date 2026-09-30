<script setup lang="ts">
// 用户与权限（user:manage）：账号 CRUD、角色、禁用、重置密码、追加授权
// （scope：作用域行编辑，覆盖语义——某权限点有作用域行即取代全局授予）、
// 在线会话查看/踢下线。新用户默认 viewer（最小权限起步）。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, Key, Plus, Refresh, SwitchButton } from '@element-plus/icons-vue'
import { api, type GroupEntry, type LabelDef, type Pool, type SessionInfo, type User } from '../api'
import { fmtTime } from '../lib/format'

const users = ref<User[]>([])
const sessions = ref<SessionInfo[]>([])
const pools = ref<Pool[]>([])
const groups = ref<GroupEntry[]>([])
const labels = ref<LabelDef[]>([])
const loading = ref(false)

// 可按作用域授权的权限点（与后端 scopeableVerbs 对应）
const SCOPEABLE = ['host:view', 'host:edit', 'run:execute', 'run:view', 'app:view', 'app:edit', 'app:upload']
const VERB_DESC: Record<string, string> = {
  'host:view': '看主机（列表/详情/指标）',
  'host:edit': '改主机（含批量设置）',
  'run:execute': '执行（应用执行/远程命令）',
  'run:view': '看执行记录（按实际触达主机收窄）',
  'app:view': '看应用',
  'app:edit': '编辑应用（保存新版本）',
  'app:upload': '上传 chart 版本',
}

async function load() {
  loading.value = true
  try {
    ;[users.value, sessions.value, pools.value, groups.value, labels.value] = await Promise.all([
      api<User[]>('GET', '/api/users'),
      api<SessionInfo[]>('GET', '/api/sessions'),
      api<Pool[]>('GET', '/api/pools'),
      api<GroupEntry[]>('GET', '/api/groups'),
      api<LabelDef[]>('GET', '/api/labels'),
    ])
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(() => load())

// ---- 建用户 ----
const createVisible = ref(false)
const createLoading = ref(false)
const createForm = reactive({ name: '', password: '', role: 'viewer' })

// 行内校验（空值交给按钮禁用态，不在这里标红）：重名在前端就能查——
// 不必等一次网络往返吃 400 "user already exists"
function nameErr(): string | null {
  const n = createForm.name.trim()
  if (!n) return null
  if (/\s/.test(createForm.name)) return '用户名不能含空白'
  if (users.value.some((u) => u.name === n)) return '用户名已存在'
  return null
}

function openCreate() {
  Object.assign(createForm, { name: '', password: '', role: 'viewer' })
  createVisible.value = true
}

async function submitCreate() {
  // 前端先拦（与后端口径一致）：空用户名/短密码不必等一次网络往返吃 400
  if (!createForm.name.trim()) {
    ElMessage.warning('请输入用户名')
    return
  }
  if (createForm.password.length < 8) {
    ElMessage.warning('密码至少 8 位')
    return
  }
  createLoading.value = true
  try {
    users.value = await api<User[]>('POST', '/api/users', { ...createForm })
    ElMessage.success(`已创建 ${createForm.name}（角色 ${createForm.role}）`)
    createVisible.value = false
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    createLoading.value = false
  }
}

// ---- 行内角色切换 / 禁用 ----
async function changeRole(row: User, role: string) {
  try {
    users.value = await api<User[]>('PUT', `/api/users/${row.id}`, { role })
    ElMessage.success(`${row.name} 角色 → ${role}`)
  } catch (e) {
    ElMessage.error((e as Error).message)
    load()
  }
}

async function toggleDisabled(row: User) {
  const to = !row.disabled
  if (to) {
    try {
      await ElMessageBox.confirm(`禁用 ${row.name}？其全部在线会话将被踢下线。`, '禁用用户', { type: 'warning' })
    } catch {
      return // 用户取消确认，不是错误
    }
  }
  // 确认与请求分开 try：请求失败要明确报错，不能与「取消」混在一个空 catch 里
  try {
    users.value = await api<User[]>('PUT', `/api/users/${row.id}`, { disabled: to })
    ElMessage.success(to ? `已禁用 ${row.name}（会话已踢）` : `已启用 ${row.name}`)
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

async function removeUser(row: User) {
  try {
    await ElMessageBox.confirm(`删除用户 ${row.name}？其追加授权一并删除。`, '删除用户', { type: 'warning' })
  } catch {
    return
  }
  try {
    users.value = await api<User[]>('DELETE', `/api/users/${row.id}`)
    ElMessage.success(`已删除 ${row.name}`)
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---- 重置密码 ----
const passVisible = ref(false)
const passLoading = ref(false)
const passUser = ref<User | null>(null)
const passForm = reactive({ password: '' })

function openReset(row: User) {
  passUser.value = row
  passForm.password = ''
  passVisible.value = true
}

async function submitReset() {
  if (!passUser.value) return
  if (passForm.password.length < 8) {
    ElMessage.warning('密码至少 8 位')
    return
  }
  passLoading.value = true
  try {
    await api('PUT', `/api/users/${passUser.value.id}/password`, { password: passForm.password })
    ElMessage.success(`已重置 ${passUser.value.name} 的密码（其旧会话已失效）`)
    passVisible.value = false
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    passLoading.value = false
  }
}

// ---- 追加授权（scope）----
const scopeVisible = ref(false)
const scopeLoading = ref(false)
const scopeUser = ref<User | null>(null)
type ScopeRow = { verb: string; kind: string; value: string }
const scopeRows = ref<ScopeRow[]>([])

const scopeValueOptions = computed(() => ({
  pool: pools.value.map((p) => p.Name),
  group: groups.value.map((g) => g.Name),
  label: labels.value.map((l) => l.Key),
}))

function openScopes(row: User) {
  scopeUser.value = row
  scopeRows.value = (row.scopes || []).map((s) => ({ verb: s.verb, kind: s.kind, value: s.value }))
  if (!scopeRows.value.length) scopeRows.value = [{ verb: 'host:edit', kind: 'pool', value: '' }]
  scopeVisible.value = true
}

// suggestFor autocomplete 建议（自由文本可输入未注册的值）
function suggestFor(row: ScopeRow) {
  return (q: string, cb: (r: { value: string }[]) => void) => {
    cb(scopeOptions(row).filter((o) => o.includes(q)).map((o) => ({ value: o })))
  }
}

function addScopeRow() {
  scopeRows.value.push({ verb: 'host:edit', kind: 'pool', value: '' })
}

function scopeOptions(row: ScopeRow): string[] {
  if (row.kind === 'pool') return scopeValueOptions.value.pool
  if (row.kind === 'group') return scopeValueOptions.value.group
  if (row.kind === 'label') return scopeValueOptions.value.label
  return []
}

async function submitScopes() {
  if (!scopeUser.value) return
  scopeLoading.value = true
  try {
    await api('PUT', `/api/users/${scopeUser.value.id}/scopes`, {
      scopes: scopeRows.value.filter((r) => r.kind === '' || r.value),
    })
    ElMessage.success(`已更新 ${scopeUser.value.name} 的追加授权`)
    scopeVisible.value = false
    await load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    scopeLoading.value = false
  }
}

async function kickSession(row: SessionInfo) {
  try {
    await api('DELETE', `/api/sessions/${row.token}`)
    ElMessage.success('已下线')
    load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}
</script>

<template>
  <div v-loading="loading">
    <div class="toolbar">
      <span class="muted">
        角色：admin（全部）· operator（执行运维）· viewer（只读）；追加授权为覆盖语义——某权限点一旦配了作用域，即取代该点的全局授予（既可提权也可收窄）
      </span>
      <div style="flex: 1" />
      <el-button :icon="Refresh" @click="load()">刷新</el-button>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建用户</el-button>
    </div>

    <el-card shadow="never" class="block">
      <template #header><span>用户</span></template>
      <el-table :data="users" size="small">
        <el-table-column prop="name" label="用户名" min-width="120" />
        <el-table-column label="角色" width="140">
          <template #default="{ row }">
            <el-select :model-value="row.role" size="small" @change="(v: string) => changeRole(row, v)">
              <el-option value="admin" label="admin" />
              <el-option value="operator" label="operator" />
              <el-option value="viewer" label="viewer" />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag v-if="row.disabled" type="danger" size="small">已禁用</el-tag>
            <el-tag v-else-if="row.online" type="success" size="small">在线</el-tag>
            <el-tag v-else type="info" size="small" effect="plain">离线</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="追加授权" min-width="240">
          <template #default="{ row }">
            <template v-if="row.scopes && row.scopes.length">
              <el-tag
                v-for="(s, i) in row.scopes" :key="i" size="small" effect="plain" class="tag"
                :type="s.kind === 'pool' ? 'warning' : s.kind === 'group' ? 'info' : 'success'"
              >
                {{ s.verb }}@{{ s.kind || '全部' }}{{ s.value ? '=' + s.value : '' }}
              </el-tag>
            </template>
            <span v-else class="muted">—</span>
          </template>
        </el-table-column>
        <el-table-column prop="created_at" label="创建时间" min-width="165">
          <template #default="{ row }"><span class="muted">{{ fmtTime(row.created_at) }}</span></template>
        </el-table-column>
        <el-table-column label="操作" width="260" fixed="right">
          <template #default="{ row }">
            <el-button link :icon="Key" @click="openScopes(row)">授权</el-button>
            <el-button link @click="openReset(row)">重置密码</el-button>
            <el-button link :type="row.disabled ? 'success' : 'warning'" @click="toggleDisabled(row)">
              {{ row.disabled ? '启用' : '禁用' }}
            </el-button>
            <el-button link type="danger" :icon="Delete" @click="removeUser(row)" />
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-card shadow="never" class="block">
      <template #header><span>在线会话（空闲 2 小时自动失效；server 重启全量失效）</span></template>
      <el-table :data="sessions" size="small">
        <el-table-column label="会话" min-width="140">
          <template #default="{ row }">
            <!-- token 明文不整串展示（截图/投屏即泄露）；截前 8 位够辨识，
                 完整值挂 title 按需查看 -->
            <span :title="row.token">{{ row.token.slice(0, 8) }}…</span>
          </template>
        </el-table-column>
        <el-table-column prop="user" label="用户" min-width="120" />
        <el-table-column prop="expires" label="过期时间" min-width="165">
          <template #default="{ row }"><span class="muted">{{ fmtTime(row.expires) }}</span></template>
        </el-table-column>
        <el-table-column label="操作" width="110" fixed="right">
          <template #default="{ row }">
            <el-button link type="danger" :icon="SwitchButton" @click="kickSession(row)">踢下线</el-button>
          </template>
        </el-table-column>
        <template #empty><el-empty description="当前无在线会话" /></template>
      </el-table>
    </el-card>

    <!-- 新建用户 -->
    <el-dialog v-model="createVisible" title="新建用户" width="440px">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom: 12px"
        title="新用户默认 viewer（只读）；最小权限起步，按需提权或配追加授权。" />
      <el-form label-width="80px" @submit.prevent>
        <el-form-item label="用户名" required>
          <el-input v-model="createForm.name" placeholder="字母数字 . _ -" />
          <span v-if="createForm.name && nameErr()" class="err">{{ nameErr() }}</span>
        </el-form-item>
        <el-form-item label="初始密码" required>
          <el-input v-model="createForm.password" type="password" show-password placeholder="至少 8 位" />
          <span v-if="createForm.password && createForm.password.length < 8" class="err">密码至少 8 位</span>
        </el-form-item>
        <el-form-item label="角色">
          <el-radio-group v-model="createForm.role">
            <el-radio-button value="viewer">viewer</el-radio-button>
            <el-radio-button value="operator">operator</el-radio-button>
            <el-radio-button value="admin">admin</el-radio-button>
          </el-radio-group>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" :loading="createLoading" :disabled="!!nameErr() || createForm.password.length < 8 || !createForm.name.trim()" @click="submitCreate">创建</el-button>
      </template>
    </el-dialog>

    <!-- 追加授权（scope 行编辑） -->
    <el-dialog v-model="scopeVisible" :title="`追加授权 · ${scopeUser?.name || ''}`" width="640px">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom: 12px"
        title="覆盖语义：某权限点一旦配了作用域行，即取代该点的全局授予——viewer + host:edit@池 是提权；给 host:view 配作用域则是把可见性收窄到该范围。" />
      <div v-for="(row, i) in scopeRows" :key="i" class="scope-row">
        <el-select v-model="row.verb" size="small" style="width: 150px">
          <el-option v-for="v in SCOPEABLE" :key="v" :value="v" :label="v">
            <span>{{ v }}</span>
            <span class="muted" style="float: right; font-size: 12px">{{ VERB_DESC[v] }}</span>
          </el-option>
        </el-select>
        <el-select v-model="row.kind" size="small" style="width: 100px" @change="row.value = ''">
          <el-option value="" label="全部" />
          <el-option value="pool" label="池" />
          <el-option value="group" label="组" />
          <el-option value="label" label="标签" />
        </el-select>
        <el-autocomplete v-if="row.kind" v-model="row.value" size="small"
          :fetch-suggestions="suggestFor(row)"
          placeholder="选择建议或输入" style="flex: 1" />
        <span v-else class="muted" style="flex: 1">全部资源</span>
        <el-button link type="danger" :icon="Delete" size="small" @click="scopeRows.splice(i, 1)" />
      </div>
      <el-button size="small" :icon="Plus" style="margin-top: 6px" @click="addScopeRow">添加授权</el-button>
      <template #footer>
        <el-button @click="scopeVisible = false">取消</el-button>
        <el-button type="primary" :loading="scopeLoading" @click="submitScopes">保存</el-button>
      </template>
    </el-dialog>

    <!-- 重置密码 -->
    <el-dialog v-model="passVisible" :title="`重置密码 · ${passUser?.name || ''}`" width="420px">
      <el-form label-width="80px">
        <el-form-item label="新密码" required>
          <el-input v-model="passForm.password" type="password" show-password placeholder="至少 8 位" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="passVisible = false">取消</el-button>
        <el-button type="primary" :loading="passLoading" @click="submitReset">重置</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.err {
  color: #dc2626;
  font-size: 12px;
}
.toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.block {
  margin-bottom: 16px;
}
.tag {
  margin: 2px 4px 2px 0;
}
.scope-row {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 8px;
}
.muted {
  color: #909399;
  font-size: 12px;
}
</style>
