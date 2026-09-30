<script setup lang="ts">
// 系统设置（admin 专属）：单文档在线改、即时生效（探活周期/会话时效/
// 告警阈值/保留策略/明文纳管）。数据库地址是启动参数（--db，缺省
// <data>/wdp.db），不在此页。乐观锁：保存带版本号，被他人改过则 409。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import { api, type SettingsDoc, type SettingsView } from '../api'
import { fmtTime } from '../lib/format'

const loading = ref(false)
const saving = ref(false)
const version = ref(0)
const meta = reactive({ updated_at: '', updated_by: '' })

// 表单：空串 = 未配置（提交时省略该字段，沿用现值/默认）。数值输入绑
// 字符串便于区分「清空」与「填 0」。
const form = reactive({
  probe_every_sec: '',
  session_ttl_min: '',
  alert_warn_pct: '',
  alert_crit_pct: '',
  metrics_retain_days: '',
  runs_retention_days: '',
  audit_retention_days: '',
  drafts_retention_days: '',
})
// 明文纳管三态（el-select 字符串哨兵）：default = 未配置（跟随启动参数）
const plaintext = ref<'default' | 'on' | 'off'>('default')

async function load() {
  loading.value = true
  try {
    const v = await api<SettingsView>('GET', '/api/settings')
    version.value = v.version
    meta.updated_at = v.updated_at || ''
    meta.updated_by = v.updated_by || ''
    const d = v as SettingsDoc
    form.probe_every_sec = d.probe_every_sec != null ? String(d.probe_every_sec) : ''
    form.session_ttl_min = d.session_ttl_min != null ? String(d.session_ttl_min) : ''
    form.alert_warn_pct = d.alert_warn_pct != null ? String(d.alert_warn_pct) : ''
    form.alert_crit_pct = d.alert_crit_pct != null ? String(d.alert_crit_pct) : ''
    form.metrics_retain_days = d.metrics_retain_days != null ? String(d.metrics_retain_days) : ''
    form.runs_retention_days = d.runs_retention_days != null ? String(d.runs_retention_days) : ''
    form.audit_retention_days = d.audit_retention_days != null ? String(d.audit_retention_days) : ''
    form.drafts_retention_days = d.drafts_retention_days != null ? String(d.drafts_retention_days) : ''
    plaintext.value = d.allow_plaintext_enroll == null ? 'default' : d.allow_plaintext_enroll ? 'on' : 'off'
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(load)

// 组装文档：只带用户显式填写的字段（空串省略——后端合并语义沿用现值）
function buildDoc(): SettingsDoc {
  const d: SettingsDoc = {}
  const num = (s: string) => (s.trim() === '' ? undefined : Number(s))
  const assign = (dst: keyof SettingsDoc, v: number | undefined) => { if (v !== undefined && !isNaN(v)) (d as Record<string, unknown>)[dst] = v }
  assign('probe_every_sec', num(form.probe_every_sec))
  assign('session_ttl_min', num(form.session_ttl_min))
  assign('alert_warn_pct', num(form.alert_warn_pct))
  assign('alert_crit_pct', num(form.alert_crit_pct))
  assign('metrics_retain_days', num(form.metrics_retain_days))
  assign('runs_retention_days', num(form.runs_retention_days))
  assign('audit_retention_days', num(form.audit_retention_days))
  assign('drafts_retention_days', num(form.drafts_retention_days))
  if (plaintext.value !== 'default') d.allow_plaintext_enroll = plaintext.value === 'on'
  return d
}

async function save() {
  saving.value = true
  try {
    const v = await api<SettingsView>('PUT', '/api/settings', { settings: buildDoc(), version: version.value })
    version.value = v.version
    ElMessage.success('设置已保存并生效')
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

const metaText = computed(() =>
  meta.updated_at ? `${meta.updated_by} · ${fmtTime(meta.updated_at)}` : '从未配置（全部使用默认）')
</script>

<template>
  <div class="settings">
    <el-card shadow="never" class="block" v-loading="loading">
      <template #header>
        <div class="head">
          <b>行为参数</b>
          <span class="muted">留空 = 使用默认/启动参数；保存后立即生效</span>
        </div>
      </template>
      <el-form label-width="200px">
        <el-form-item label="探活周期（秒，5–3600）">
          <el-input v-model="form.probe_every_sec" placeholder="默认 30" style="width: 200px" />
        </el-form-item>
        <el-form-item label="会话空闲失效（分钟，5–1440）">
          <el-input v-model="form.session_ttl_min" placeholder="默认 120" style="width: 200px" />
        </el-form-item>
        <el-form-item label="告警 warn 阈值（%，50–99）">
          <el-input v-model="form.alert_warn_pct" placeholder="默认 85（文件系统 +3）" style="width: 200px" />
        </el-form-item>
        <el-form-item label="告警 crit 阈值（%，> warn）">
          <el-input v-model="form.alert_crit_pct" placeholder="默认 95" style="width: 200px" />
        </el-form-item>
        <el-form-item label="指标保留（天，1–3650）">
          <el-input v-model="form.metrics_retain_days" placeholder="默认 30" style="width: 200px" />
        </el-form-item>
        <el-form-item label="执行记录保留（天，0=永久）">
          <el-input v-model="form.runs_retention_days" placeholder="默认 90" style="width: 200px" />
        </el-form-item>
        <el-form-item label="审计日志保留（天，0=永久）">
          <el-input v-model="form.audit_retention_days" placeholder="默认 180" style="width: 200px" />
        </el-form-item>
        <el-form-item label="编辑器草稿保留（天，0=永久）">
          <el-input v-model="form.drafts_retention_days" placeholder="默认 30" style="width: 200px" />
        </el-form-item>
        <el-form-item label="明文 HTTP 纳管">
          <el-select v-model="plaintext" style="width: 200px">
            <el-option label="跟随启动参数" value="default" />
            <el-option label="允许（可信内网）" value="on" />
            <el-option label="禁止" value="off" />
          </el-select>
        </el-form-item>
      </el-form>
      <div class="foot">
        <span class="muted">最近修改：{{ metaText }}</span>
        <div>
          <el-button :icon="Refresh" @click="load">刷新</el-button>
          <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        </div>
      </div>
    </el-card>

  </div>
</template>

<style scoped>
.settings { max-width: 900px; }
.block { margin-bottom: 16px; }
.head { display: flex; align-items: baseline; gap: 12px; }
.foot { display: flex; justify-content: space-between; align-items: center; }
</style>
