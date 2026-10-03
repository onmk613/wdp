<script setup lang="ts">
// 编辑主机对话框（改地址/池/组/标签/明文通道），自 HostsPage 拆出。
// 打开时父级先把目标行放进 host prop、再置 visible=true；表单据此回填
//（与拆出前 openEdit 的 Object.assign 回填同口径）。
import { reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { api, type Host } from '../../api'
import LabelRows from '../LabelRows.vue'
import { parseLabels } from '../../lib/format'

const props = defineProps<{
  host: Host | null
  poolNames: string[]
  groupNames: string[]
  labelKeys: string[]
}>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ (e: 'saved'): void }>()

const editID = ref(0)
// 字段口径：表单与 API 请求体统一 snake_case（与响应口径一致；后端解码
// 大小写不敏感，纯风格统一）
const form = reactive({ name: '', address: '', agent_port: 7602, pools: [] as string[], groups: [] as string[], allow_plaintext: false })
const labels = ref<Record<string, string>>({})
const loading = ref(false)

watch(visible, (v) => {
  if (!v || !props.host) return
  editID.value = props.host.id
  Object.assign(form, {
    name: props.host.name,
    address: props.host.address,
    agent_port: props.host.agent_port,
    pools: props.host.pools || [],
    groups: props.host.groups || [],
    allow_plaintext: !!props.host.allow_plaintext,
  })
  labels.value = parseLabels(props.host.labels)
})

async function submit() {
  loading.value = true
  try {
    await api('PUT', `/api/hosts/${editID.value}`, {
      address: form.address,
      agent_port: form.agent_port,
      pools: form.pools,
      groups: form.groups,
      labels: JSON.stringify(labels.value || {}),
      allow_plaintext: form.allow_plaintext,
    })
    ElMessage.success(`已更新 ${form.name}`)
    visible.value = false
    emit('saved')
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" :title="`编辑主机 ${form.name}`" width="520px">
    <el-form label-width="90px">
      <el-form-item label="地址">
        <el-input v-model="form.address" />
      </el-form-item>
      <el-form-item label="agent 端口">
        <el-input-number v-model="form.agent_port" :min="1" :max="65535" />
      </el-form-item>
      <el-form-item label="池（多选）">
        <el-select v-model="form.pools" multiple style="width: 100%">
          <el-option v-for="p in poolNames" :key="p" :label="p" :value="p" />
        </el-select>
      </el-form-item>
      <el-form-item label="组（多选）">
        <el-select v-model="form.groups" multiple style="width: 100%">
          <el-option v-for="g in groupNames" :key="g" :label="g" :value="g" />
        </el-select>
      </el-form-item>
      <el-form-item label="标签">
        <LabelRows v-model="labels" :keys="labelKeys" />
      </el-form-item>
      <el-form-item label="明文通道">
        <el-switch v-model="form.allow_plaintext" />
        <div class="muted" style="margin-left: 12px; line-height: 1.6">
          仅当该主机的 agent 未启用 mTLS 时打开（可信内网）。<br />
          默认关闭：控制台一律按 mTLS 建连并校验证书身份；<br />
          明文通道下脚本、口令与制品在网络上不加密。
        </div>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="loading" @click="submit">保存</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.muted {
  color: #909399;
  font-size: 12px;
}
</style>
