<script setup lang="ts">
// 新建池 / 组 / 标签（同一组件按 kind 实例化三次——三张表单同构：名称 +
// 备注 + 划入已有主机，仅接口与提示文案不同），自 HostsPage 拆出
//（该页此前内联 6 个对话框）。visible 经 defineModel 双向绑定；表单在
// 每次打开时重置（与拆出前 openPool/openGroup/openLabel 里 Object.assign
// 重置同口径）。
import { reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { api, type Host } from '../../api'

const props = defineProps<{
  kind: 'pool' | 'group' | 'label'
  hosts: Host[] // 划入主机候选（台账当前列表）
  existingNames: string[] // 重名校验：池名 / 组名 / 标签键
}>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ (e: 'created'): void }>()

const kindMeta = {
  pool: {
    title: '新建主机池', nameLabel: '池名', requiredMsg: '池名必填', namePlaceholder: '如 prod / test',
    hostsLabel: '划入主机', hostsPlaceholder: '可选：选择已有主机划入该池',
    dupMsg: (n: string) => `池 "${n}" 已存在（主机池下拉里可见）；如需调整成员请编辑主机或批量设置`,
    okMsg: (n: string, c: number) => `已建池 ${n}（${c} 台主机划入）`,
    endpoint: '/api/pools',
    body: (name: string, value: string, note: string, hostIDs: number[]) => ({ name, note, host_ids: hostIDs }),
  },
  group: {
    title: '新建组', nameLabel: '组名', requiredMsg: '组名必填', namePlaceholder: '如 web / db',
    hostsLabel: '划入主机', hostsPlaceholder: '可选：选择已有主机加入该组',
    dupMsg: (n: string) => `组 "${n}" 已存在（组下拉里可见）`,
    okMsg: (n: string, c: number) => `已建组 ${n}（${c} 台主机划入）`,
    endpoint: '/api/groups',
    body: (name: string, value: string, note: string, hostIDs: number[]) => ({ name, note, host_ids: hostIDs }),
  },
  label: {
    title: '新建标签', nameLabel: '标签键', requiredMsg: '标签键必填', namePlaceholder: '如 env / tier',
    hostsLabel: '附加主机', hostsPlaceholder: '可选：为已有主机加上该标签',
    dupMsg: (n: string) => `标签 "${n}" 已存在（可在主机编辑里直接使用）`,
    okMsg: (n: string, c: number) => `已建标签 ${n}（${c} 台主机附加）`,
    endpoint: '/api/labels',
    body: (name: string, value: string, note: string, hostIDs: number[]) => ({ key: name, value, note, host_ids: hostIDs }),
  },
} as const
const meta = () => kindMeta[props.kind]

const form = reactive({ name: '', value: '', note: '', hostIDs: [] as number[] })
const loading = ref(false)

watch(visible, (v) => {
  if (v) Object.assign(form, { name: '', value: '', note: '', hostIDs: [] })
})

async function submit() {
  const m = meta()
  const name = form.name.trim()
  if (!name) {
    ElMessage.warning(m.requiredMsg)
    return
  }
  if (props.existingNames.includes(name)) {
    ElMessage.warning(m.dupMsg(name))
    return
  }
  loading.value = true
  try {
    await api('POST', m.endpoint, m.body(form.name.trim(), form.value, form.note, form.hostIDs))
    ElMessage.success(m.okMsg(form.name, form.hostIDs.length))
    visible.value = false
    emit('created')
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" :title="meta().title" width="520px">
    <el-form label-width="110px">
      <el-form-item :label="meta().nameLabel" required>
        <el-input v-model="form.name" :placeholder="meta().namePlaceholder" />
      </el-form-item>
      <el-form-item v-if="kind === 'label'" label="默认值">
        <el-input v-model="form.value" placeholder="可选（附加主机时写入的值）" />
      </el-form-item>
      <el-form-item label="备注">
        <el-input v-model="form.note" />
      </el-form-item>
      <el-form-item :label="meta().hostsLabel">
        <el-select v-model="form.hostIDs" multiple :placeholder="meta().hostsPlaceholder" style="width: 100%">
          <el-option v-for="hst in hosts" :key="hst.ID" :label="`${hst.Name} (${hst.Address})`" :value="hst.ID" />
        </el-select>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="loading" @click="submit">创建</el-button>
    </template>
  </el-dialog>
</template>
