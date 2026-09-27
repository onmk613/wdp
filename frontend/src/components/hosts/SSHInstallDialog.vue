<script setup lang="ts">
// SSH 批量推装对话框（自 HostsPage 拆出——该页曾内嵌 8 个对话框、
// 1200 行）：共享凭据 + 多行主机清单，并发 3 逐台安装。
// 本组件经 v-if 挂载：关闭即卸载，共享密码/私钥口令与含明文密码的清单
// 随组件销毁，不残留页面内存（HostsPage 随 Console 布局常驻）。
import { computed, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '../../api'
import { downloadText, parseSSHCSV, readCSVFile, sshCSVTemplate, type SSHCSVRow } from '../../csv'

const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ (e: 'installed'): void }>()

const sshText = ref('')
const sshFileInput = ref()
// 共享连接设置：应用到清单里 user/password 留空的行（行内值优先）。
// 指纹校验默认开（与后端安全默认一致）；关闭是知情选择（MITM 可截获
// SSH 凭据并替换 agent 二进制）
const sshShared = reactive({
  user: '', password: '', key_path: '', key_passphrase: '',
  verify_host_key: true, agent_port: 7602,
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
const sshFailedN = computed(() => sshJobs.value.filter((j) => j.status === 'failed').length)

function onSSHFilePicked(e: Event) {
  const input = e.target as HTMLInputElement
  const f = input.files?.[0]
  if (f) readCSVFile(f).then((t) => (sshText.value = t.trim()))
  input.value = ''
}

// 安装进行中不允许关对话框（关页面=放弃进度，服务端安装仍在跑）；
// 允许关闭时由父级 v-if 卸载本组件，凭据随之销毁
function beforeClose(done: () => void) {
  if (sshRunning.value) {
    ElMessage.warning('安装进行中，请等待完成')
    return
  }
  done()
}

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
  emit('installed')
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
</script>

<template>
  <el-dialog
    :model-value="visible"
    title="SSH 安装 agent（批量）"
    width="800px"
    :before-close="beforeClose"
    :close-on-click-modal="!sshRunning"
    @update:model-value="visible = $event"
  >
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
        <span class="muted" style="margin-left: 8px">默认开：校验 known_hosts 指纹，防中间人截获凭据/替换二进制；关闭前需预先采集指纹</span>
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
      <el-button :disabled="sshRunning" @click="visible = false">关闭</el-button>
      <el-button v-if="sshFailedN && !sshRunning" type="warning" @click="retrySSHFailed">
        重试失败 {{ sshFailedN }} 台
      </el-button>
      <el-button v-if="!sshJobs.length" type="primary" :disabled="!sshValidN" @click="submitSSHInstall">
        安装 {{ sshValidN }} 台（含上传二进制，每台约一分钟）
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.muted { color: #909399; font-size: 12px; }
.csv-err { color: #dc2626; font-size: 12px; }
</style>
