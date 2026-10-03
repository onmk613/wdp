<script setup lang="ts">
// 手动添加主机对话框（多行文本 / CSV 批量导入），自 HostsPage 拆出。
// 每行一台：name,address,agent_port,pools,groups,labels；粘贴或上传后
// 下方实时预览并校验。
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, type BatchResponse } from '../../api'
import { downloadText, hostCSVTemplate, parseHostCSV, readCSVFile } from '../../csv'

const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ (e: 'imported'): void }>()

const loading = ref(false)
const text = ref('')
const fileInput = ref()

const rows = computed(() => parseHostCSV(text.value))
const validN = computed(() => rows.value.filter((r) => r.value).length)

function labelsText(labels: Record<string, string>): string {
  return Object.entries(labels).map(([k, v]) => (v ? `${k}=${v}` : k)).join('; ')
}

function onFilePicked(e: Event) {
  const input = e.target as HTMLInputElement
  const f = input.files?.[0]
  if (f) readCSVFile(f).then((t) => (text.value = t.trim()))
  input.value = ''
}

async function submit() {
  const rs = rows.value.filter((r) => r.value)
  if (!rs.length) {
    ElMessage.warning('没有可导入的主机行（先按模版格式填写）')
    return
  }
  loading.value = true
  try {
    const r = await api<BatchResponse>('POST', '/api/hosts/import', {
      hosts: rs.map(({ value }) => ({
        name: value!.name,
        address: value!.address,
        agent_port: value!.agentPort,
        pools: value!.pools,
        groups: value!.groups,
        labels: value!.labels,
      })),
    })
    visible.value = false
    emit('imported')
    if (r.failed > 0) {
      const bad = r.results
        .filter((x) => !x.ok)
        .map((x) => `第 ${rs[(x.row ?? 1) - 1]?.line ?? '?'} 行 ${x.name}：${x.detail}`)
        .join('\n')
      await ElMessageBox.alert(bad, `导入完成：成功 ${r.ok} 台，失败 ${r.failed} 台`, { type: 'warning' })
    } else {
      ElMessage.success(`已导入 ${r.ok} 台主机`)
    }
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" title="手动添加主机（多行 / CSV 批量）" width="760px">
    <el-alert type="info" :closable="false" show-icon style="margin-bottom: 10px"
      title="每行一台：name,address,agent_port,pools,groups,labels；整行只写一个 IP 也可以；池/组/标签多值用分号分隔；name 与端口留空 = address / 7602" />
    <div class="csv-actions">
      <el-button size="small" @click="downloadText('wdp-hosts.csv', hostCSVTemplate)">下载 CSV 模版</el-button>
      <el-button size="small" @click="fileInput?.click()">上传 CSV 文件</el-button>
      <span class="muted">粘贴或上传后可继续编辑，下方实时预览并校验</span>
      <input ref="fileInput" type="file" accept=".csv,text/csv" hidden @change="onFilePicked" />
    </div>
    <el-input
      v-model="text" type="textarea" :rows="7" spellcheck="false"
      placeholder="web1,192.168.1.11,7602,web;prod,api,env=prod;rack=a1&#10;web2,192.168.1.12&#10;db1,192.168.1.21,,,db,"
    />
    <el-table v-if="rows.length" :data="rows" size="small" border max-height="220" style="margin-top: 10px">
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
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="loading" :disabled="!validN" @click="submit">
        导入 {{ validN }} 台主机
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
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
</style>
