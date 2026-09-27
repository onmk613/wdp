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
const form = reactive({ Name: '', Address: '', AgentPort: 7602, Pools: [] as string[], Groups: [] as string[], AllowPlaintext: false })
const labels = ref<Record<string, string>>({})
const loading = ref(false)

watch(visible, (v) => {
  if (!v || !props.host) return
  editID.value = props.host.ID
  Object.assign(form, {
    Name: props.host.Name,
    Address: props.host.Address,
    AgentPort: props.host.AgentPort,
    Pools: props.host.Pools || [],
    Groups: props.host.Groups || [],
    AllowPlaintext: !!props.host.AllowPlaintext,
  })
  labels.value = parseLabels(props.host.Labels)
})

async function submit() {
  loading.value = true
  try {
    await api('PUT', `/api/hosts/${editID.value}`, {
      Address: form.Address,
      AgentPort: form.AgentPort,
      Pools: form.Pools,
      Groups: form.Groups,
      Labels: JSON.stringify(labels.value || {}),
      AllowPlaintext: form.AllowPlaintext,
    })
    ElMessage.success(`已更新 ${form.Name}`)
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
  <el-dialog v-model="visible" :title="`编辑主机 ${form.Name}`" width="520px">
    <el-form label-width="90px">
      <el-form-item label="地址">
        <el-input v-model="form.Address" />
      </el-form-item>
      <el-form-item label="agent 端口">
        <el-input-number v-model="form.AgentPort" :min="1" :max="65535" />
      </el-form-item>
      <el-form-item label="池（多选）">
        <el-select v-model="form.Pools" multiple style="width: 100%">
          <el-option v-for="p in poolNames" :key="p" :label="p" :value="p" />
        </el-select>
      </el-form-item>
      <el-form-item label="组（多选）">
        <el-select v-model="form.Groups" multiple style="width: 100%">
          <el-option v-for="g in groupNames" :key="g" :label="g" :value="g" />
        </el-select>
      </el-form-item>
      <el-form-item label="标签">
        <LabelRows v-model="labels" :keys="labelKeys" />
      </el-form-item>
      <el-form-item label="明文通道">
        <el-switch v-model="form.AllowPlaintext" />
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
