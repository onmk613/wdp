<script setup lang="ts">
// IDE 目录树：chart 根 + 文件/目录层级，顶部工具条（新建文件/目录/相位、
// 上传），右键菜单（打开/重命名/删除/撤销删除）。lint 问题以红/黄角点
// 标在文件节点上。文件操作直接作用于 ChartFS，完成后 emit('op') 让
// AppIDE 同步 Monaco model/标签页。
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { FolderAdd, Plus, Upload, MagicStick, View } from '@element-plus/icons-vue'
import type { ChartFS } from '../../ide/fs'
import { isReserved, pathOk, SPEC_TEXT_LIMIT } from '../../ide/fs'
import type { ProblemItem } from '../../ide/validate'

const props = defineProps<{ fs: ChartFS; problems: ProblemItem[]; loading?: boolean }>()
const emit = defineEmits<{
  (e: 'open', path: string): void
  (e: 'op', ev: { kind: string; path: string; to?: string; prevContent?: string }): void
}>()

// 手工新建的空目录（无文件时仅存在于树里；首个文件落进去才实际成形）
const extraDirs = ref(new Set<string>())
const selected = ref('')

// 目录展开状态：treeData 每次 fs 变更都会整体重建节点对象，el-tree 对
// 重建的节点回到折叠态（default-expanded-keys 之外）——不跟踪展开集合
// 的话，目录里新建/移入文件后目录"吞掉"文件，看起来像操作失败
const expandedDirs = ref(new Set<string>(['']))
const expandedKeys = computed(() => [...expandedDirs.value])
function onNodeExpand(data: TreeNode) {
  if (!data.isDir) return
  expandedDirs.value = new Set(expandedDirs.value).add(data.path)
}
function onNodeCollapse(data: TreeNode) {
  if (!data.isDir) return
  const s = new Set(expandedDirs.value)
  s.delete(data.path)
  expandedDirs.value = s
}
// 展开路径的全部祖先目录（新建/移动/上传后调用，让落点立即可见）
function expandTo(path: string) {
  const segs = path.split('/')
  if (segs.length < 2) return
  const s = new Set(expandedDirs.value)
  for (let i = 1; i < segs.length; i++) s.add(segs.slice(0, i).join('/'))
  expandedDirs.value = s
}

interface TreeNode {
  label: string
  path: string // 目录 = 路径前缀；文件 = 完整路径
  isDir: boolean
  children?: TreeNode[]
  deleted?: boolean
  binary?: boolean
}

const ROOT_ORDER = ['chart.yaml', 'values.yaml', 'deploy.yaml']

const treeData = computed<TreeNode[]>(() => {
  const root: TreeNode = { label: props.fs.name || 'chart', path: '', isDir: true, children: [] }
  const dirIndex = new Map<string, TreeNode>([['', root]])
  const ensureDir = (dir: string): TreeNode => {
    if (dirIndex.has(dir)) return dirIndex.get(dir)!
    // 一级目录的父是根（''）；多级目录的父是上一段路径。
    // 不能用 slice(0, lastIndexOf('/'))：一级目录无斜杠时会得到
    // "file"→"fil"→… 的逐字符嵌套
    const parent = ensureDir(dir.includes('/') ? dir.slice(0, dir.lastIndexOf('/')) : '')
    const node: TreeNode = { label: dir.slice(dir.lastIndexOf('/') + 1), path: dir, isDir: true, children: [] }
    parent.children!.push(node)
    dirIndex.set(dir, node)
    return node
  }
  for (const d of extraDirs.value) ensureDir(d)
  const files = props.fs.allFiles()
  for (const f of files) {
    const dir = f.path.includes('/') ? f.path.slice(0, f.path.lastIndexOf('/')) : ''
    ensureDir(dir).children!.push({
      label: f.path.slice(f.path.lastIndexOf('/') + 1),
      path: f.path, isDir: false, deleted: f.deleted, binary: f.binary,
    })
  }
  const sortNodes = (nodes: TreeNode[]) => {
    nodes.sort((a, b) => {
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
      const ra = ROOT_ORDER.indexOf(a.path), rb = ROOT_ORDER.indexOf(b.path)
      if (ra >= 0 || rb >= 0) return (ra < 0 ? 99 : ra) - (rb < 0 ? 99 : rb)
      return a.label.localeCompare(b.label)
    })
    nodes.forEach((n) => sortNodes(n.children || []))
  }
  sortNodes(root.children || [])
  return [root]
})

onMounted(() => {
  document.addEventListener('click', closeCtx)
})
onBeforeUnmount(() => document.removeEventListener('click', closeCtx))

// ---- 右键菜单 ----
const ctxMenu = reactive({ visible: false, x: 0, y: 0, node: null as TreeNode | null })
// element-plus node-context-menu 回调参数为 (event, data, node, instance)：
// 第 2 参才是树节点的 data（TreeNode），第 3 参是内部 Node 包装，不可用
function onCtx(e: MouseEvent, data: TreeNode) {
  e.preventDefault()
  ctxMenu.visible = true
  ctxMenu.x = e.clientX
  ctxMenu.y = e.clientY
  ctxMenu.node = data
  if (!data.isDir) selected.value = data.path
}
function closeCtx() {
  ctxMenu.visible = false
  ctxMenu.node = null // 清掉选中节点：否则工具栏「上传」会落到上次右键的目录
}
function ctxIs(kind: string): boolean {
  const n = ctxMenu.node
  if (!n) return false
  if (kind === 'file') return !n.isDir && !n.deleted
  if (kind === 'deleted') return !n.isDir && !!n.deleted
  if (kind === 'dir') return n.isDir
  return false
}

// 当前选中目录（新建文件的落点）
function targetDir(): string {
  const n = ctxMenu.node
  if (!n) return ''
  return n.isDir ? n.path : (n.path.includes('/') ? n.path.slice(0, n.path.lastIndexOf('/')) : '')
}

const fileIcon = (path: string): string => {
  if (isReserved(path)) return '★'
  if (/\.tpl$/.test(path)) return 'tpl'
  if (/\.ya?ml$/.test(path)) return 'yaml'
  if (/\.json$/.test(path)) return 'json'
  if (/\.(sh|py)$/.test(path)) return '>'
  if (/\.conf$|\.ini$|\.env$|\.service$/.test(path)) return 'cfg'
  return '·'
}

// ---- 问题角标 ----
const problemCount = computed(() => {
  const m = new Map<string, { error: number; warn: number }>()
  for (const p of props.problems) {
    if (!p.path) continue
    const cur = m.get(p.path) || { error: 0, warn: 0 }
    if (p.level === 'ERROR') cur.error++
    else cur.warn++
    m.set(p.path, cur)
  }
  return m
})

// ---- 操作 ----
function askPath(title: string, initial: string, validator?: (v: string) => string | null): Promise<string | null> {
  return ElMessageBox.prompt(title, {
    inputValue: initial,
    inputPlaceholder: '相对 chart 根，如 files/app.conf 或 tasks/init.yaml',
    confirmButtonText: '确定',
    cancelButtonText: '取消',
    inputValidator: (v: string) => validator ? (validator(v.trim()) || true) : (pathOk(v.trim()) ? true : '路径不合法'),
  }).then((r) => (r.value as string).trim()).catch(() => null)
}

// ---- 约定文件/目录（快捷创建；先约定后自定义） ----

// 约定文件：带用途说明与内容脚手架
const QUICK_FILES: { path: string; desc: string; content: string }[] = [
  {
    path: '_helpers.tpl', desc: '模板辅助：命名模板集中定义，任务/配置模板 include 引用',
    content: [
      '{{/* 模板辅助：define 定义命名模板，别处用 include "名字" . 引用 */}}',
      '{{- define "app.name" -}}',
      '{{ .name | default "myapp" }}',
      '{{- end -}}',
      '',
    ].join('\n'),
  },
  {
    path: 'values.schema.json', desc: 'values 结构校验（编辑器实时生效）',
    content: [
      '{',
      '  "$schema": "http://json-schema.org/draft/2020-12/schema",',
      '  "type": "object",',
      '  "title": "chart values",',
      '  "additionalProperties": true,',
      '  "properties": {}',
      '}',
      '',
    ].join('\n'),
  },
  {
    path: 'tasks/main.yaml', desc: '任务片段（include: tasks/main.yaml 引用）',
    content: [
      '# 任务片段：deploy 里 include: tasks/main.yaml 静态展开',
      '- name: 第一个任务',
      "  shell: 'echo hello'",
      '',
    ].join('\n'),
  },
  {
    path: 'handlers/restart-docker.yaml', desc: '处理器片段（play 的 handlers: include 引用；notify 触发）',
    content: [
      '# 处理器片段：play 里 handlers: [- include: handlers/restart-docker.yaml] 引入，',
      '# 任务 notify: 重启 docker 触发，play 末尾统一 flush',
      '- name: 重启 docker',
      '  service:',
      '    name: docker',
      '    state: restarted',
      '',
    ].join('\n'),
  },
  {
    path: 'envs/prod.yaml', desc: '环境 values 覆盖（执行时选择 / values_from 引用）',
    content: ['# prod 环境覆盖：与 values.yaml 深合并（本文件优先）\n{}\n'].join(''),
  },
  {
    path: 'templates/app.conf.tpl', desc: '配置模板（template 模块下发，{{ .变量 }} 引 values）',
    content: [
      '{{/* 配置模板：template: {src: templates/app.conf.tpl, dest: /etc/app/app.conf} */}}',
      '# {{ .app.name | default "myapp" }} 配置',
      '',
    ].join('\n'),
  },
]

// 约定目录
const QUICK_DIRS: { path: string; desc: string }[] = [
  { path: 'files', desc: '随包静态文件（copy 的 src 引用）' },
  { path: 'templates', desc: '配置模板（.tpl）' },
  { path: 'tasks', desc: '任务片段（include 引用）' },
  { path: 'handlers', desc: '处理器片段（play 的 handlers: include 引用；notify 触发）' },
  { path: 'charts', desc: '子 chart（chart 引用）' },
  { path: 'envs', desc: '环境 values 覆盖文件' },
  { path: 'modules', desc: 'chart 本地脚本模块（可执行）' },
]

// 新建文件弹窗：约定列表 + 自定义路径
const newFileVisible = ref(false)
const newFileCustom = ref('')
const newFileInitialDir = ref('')
function openNewFile(dir = '') {
  newFileInitialDir.value = dir
  newFileCustom.value = dir ? `${dir}/` : ''
  newFileVisible.value = true
}
function quickFileExists(path: string): boolean {
  const f = props.fs.get(path)
  return !!f && !f.deleted
}
function createFileAt(path: string, content: string) {
  const err = props.fs.create(path, content)
  if (err) { ElMessage.warning(err); return }
  // 父目录若只是手工登记的空目录，现在有真实文件了
  newFileVisible.value = false
  expandTo(path)
  emit('op', { kind: 'create', path })
  emit('open', path)
}
function createQuickFile(q: { path: string; content: string }) {
  if (quickFileExists(q.path)) { ElMessage.warning(`${q.path} 已存在`); return }
  createFileAt(q.path, q.content)
}
function createCustomFile() {
  const path = newFileCustom.value.trim()
  if (!pathOk(path)) { ElMessage.warning('路径不合法（相对 chart 根，禁 .. 与绝对路径）'); return }
  const content = /\.ya?ml$/.test(path) && /^(tasks|handlers)\//.test(path) ? '- shell: echo ok\n' : ''
  createFileAt(path, content)
}

// 新建目录弹窗：约定目录 + 自定义
const newDirVisible = ref(false)
const newDirCustom = ref('')
function openNewDir() {
  newDirCustom.value = ''
  newDirVisible.value = true
}
function dirExists(path: string): boolean {
  if (extraDirs.value.has(path)) return true
  for (const f of props.fs.allFiles()) {
    if (!f.deleted && f.path.startsWith(path + '/')) return true
  }
  return false
}
function addDir(path: string) {
  if (dirExists(path)) { ElMessage.warning(`${path}/ 已存在`); return }
  extraDirs.value = new Set([...extraDirs.value, path])
  newDirVisible.value = false
}
function addCustomDir() {
  const path = newDirCustom.value.trim().replace(/\/+$/, '')
  if (!pathOk(path + '/x')) { ElMessage.warning('目录名不合法'); return }
  addDir(path)
}

// 新建相位文件：根目录 <相位名>.yaml + 询问是否在 chart.yaml 声明
async function newPhase() {
  const name = await askPath('新建相位（根目录 <相位名>.yaml，如 uninstall/backup/reload）', '', (v) =>
    /^[A-Za-z0-9_-]+$/.test(v) ? null : '相位名限字母数字 _ -')
  if (!name) return
  const path = `${name}.yaml`
  if (props.fs.get(path)) { ElMessage.warning('文件已存在'); return }
  props.fs.create(path, `# ${name} 相位定义\n- name: ${name}\n  hosts: {{.hosts}}\n  tasks: []\n`.replace('{{.hosts}}', props.fs.name || 'all'))
  emit('op', { kind: 'create', path })
  emit('open', path)
  try {
    await ElMessageBox.confirm(
      `是否同时在 chart.yaml 的 phases: 里声明 ${name}？（声明后才有部署语义/marker 等属性）`,
      '声明相位',
      { confirmButtonText: '声明', cancelButtonText: '跳过', type: 'info' },
    )
    const chart = props.fs.get('chart.yaml')
    if (chart) {
      const prev = chart.content
      const patched = patchPhases(chart.content, name)
      if (patched === null) {
        ElMessage.warning(`chart.yaml 的 phases 是行内（flow）写法，请手动添加 ${name}: {} 声明`)
      } else {
        chart.content = patched
        emit('op', { kind: 'edit', path: 'chart.yaml', prevContent: prev })
      }
    }
  } catch { /* 跳过声明 */ }
}

// patchPhases 在 chart.yaml 追加相位声明。块状 `phases:`（含已有子项）在
// 其下插入；行内（flow）写法 `phases: {…}` 返回 null——正则插入会造出
// 第二个顶层 phases: 键，yaml 重复键解析失败，宁可不改提示手动处理。
function patchPhases(chartYAML: string, phase: string): string | null {
  if (/^phases:[ \t]*$/m.test(chartYAML)) {
    return chartYAML.replace(/^phases:[ \t]*$/m, `phases:\n  ${phase}: {}`)
  }
  if (/^phases:/m.test(chartYAML)) return null
  return `${chartYAML.trimEnd()}\nphases:\n  ${phase}: {}\n`
}

async function renameFile(node: TreeNode) {
  const path = await askPath('重命名 / 移动', node.path)
  if (!path || path === node.path) return
  const err = props.fs.rename(node.path, path)
  if (err) { ElMessage.warning(err); return }
  expandTo(path)
  emit('op', { kind: 'rename', path: node.path, to: path })
}

async function deleteFile(node: TreeNode) {
  try {
    await ElMessageBox.confirm(`删除 ${node.path}？（保存时生效，可撤销）`, '删除文件', {
      confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning',
    })
  } catch { return }
  const err = props.fs.remove(node.path)
  if (err) { ElMessage.warning(err); return }
  emit('op', { kind: 'delete', path: node.path })
}

function restoreFile(node: TreeNode) {
  const err = props.fs.restore(node.path)
  if (err) { ElMessage.warning(err); return }
  emit('op', { kind: 'restore', path: node.path })
}

// ---- 上传 ----
const picker = ref<HTMLInputElement | null>(null)
function pickUpload() { picker.value?.click() }

async function onUploadPicked(e: Event) {
  const input = e.target as HTMLInputElement
  const files = [...(input.files || [])]
  input.value = ''
  const dir = targetDir() || 'files'
  let okCount = 0
  for (const file of files) {
    // 与后端读侧单文件文本上限（512KiB）一致：更大的文件保存后重开会被
    // 判成二进制只读，这里直接拦住
    if (file.size > SPEC_TEXT_LIMIT) {
      ElMessage.error(`${file.name} 超过 512KiB：chart 内文本文件上限（更大文件请走 tgz 上传）`)
      continue
    }
    const text = await file.text()
    if (text.includes('\0')) {
      ElMessage.error(`${file.name} 是二进制文件，已拒绝`)
      continue
    }
    const path = `${dir}/${file.name}`
    const err = props.fs.create(path, text)
    if (err) { ElMessage.warning(`${file.name}: ${err}`); continue }
    expandTo(path)
    emit('op', { kind: 'create', path })
    okCount++
  }
  if (okCount) ElMessage.success(`已加入 ${okCount} 个文件`)
}

function onNodeClick(data: TreeNode) {
  if (data.isDir || data.deleted || data.binary) {
    if (data.binary && !data.deleted) ElMessage.info(`${data.path} 是二进制文件（随包保留，不可编辑）`)
    return
  }
  emit('open', data.path)
}
</script>

<template>
  <div class="file-tree">
    <div class="tree-tools">
      <el-tooltip content="新建文件（约定文件 + 自定义）" placement="bottom"><el-button :icon="Plus" size="small" text bg @click="openNewFile()" /></el-tooltip>
      <el-tooltip content="新建目录（约定目录 + 自定义）" placement="bottom"><el-button :icon="FolderAdd" size="small" text bg @click="openNewDir" /></el-tooltip>
      <el-tooltip content="新建相位（<相位名>.yaml + chart.yaml 声明）" placement="bottom"><el-button :icon="MagicStick" size="small" text bg @click="newPhase" /></el-tooltip>
      <el-tooltip content="上传文本文件" placement="bottom"><el-button :icon="Upload" size="small" text bg @click="pickUpload" /></el-tooltip>
      <el-tooltip content="打开当前选中文件" placement="bottom"><el-button :icon="View" size="small" text bg :disabled="!selected" @click="emit('open', selected)" /></el-tooltip>
      <input ref="picker" type="file" multiple hidden
        accept=".txt,.conf,.cfg,.ini,.env,.yaml,.yml,.json,.tpl,.xml,.md,.sh,.service,.py,.sql"
        @change="onUploadPicked" />
    </div>
    <el-scrollbar class="tree-scroll">
      <el-tree
        :data="treeData"
        node-key="path"
        :default-expanded-keys="expandedKeys"
        :expand-on-click-node="true"
        :highlight-current="true"
        :props="{ label: 'label', children: 'children' }"
        @node-click="onNodeClick"
        @node-context-menu="onCtx"
        @node-expand="onNodeExpand"
        @node-collapse="onNodeCollapse"
      >
        <template #default="{ data }">
          <span class="node" :class="{ deleted: data.deleted, dir: data.isDir, binary: data.binary }">
            <span v-if="!data.isDir" class="fico" :class="{ core: isReserved(data.path) }">{{ fileIcon(data.path) }}</span>
            <span class="nlabel">{{ data.label }}</span>
            <span v-if="data.deleted" class="tag del">删</span>
            <span v-else-if="data.binary" class="tag bin">bin</span>
            <template v-if="problemCount.get(data.path)">
              <span v-if="problemCount.get(data.path)!.error" class="ptag err" :title="`${problemCount.get(data.path)!.error} 个错误`">●</span>
              <span v-else class="ptag warn" :title="`${problemCount.get(data.path)!.warn} 个警告`">●</span>
            </template>
            <!-- 悬停操作按钮：右键菜单在内嵌浏览器里常调不出来，这里是
                 删除/重命名的主要入口（三件套不显示）。目录给 ➕（在此
                 新建文件），文件给 ✎/✕，已删文件给 ↩ -->
            <span v-if="data.isDir" class="node-actions" @click.stop @mousedown.stop>
              <span class="act" title="在此新建文件" @click.stop="openNewFile(data.path)">＋</span>
            </span>
            <span v-else-if="!data.isDir && !data.deleted && !isReserved(data.path)" class="node-actions" @click.stop @mousedown.stop>
              <span class="act" title="重命名 / 移动" @click.stop="renameFile(data)">✎</span>
              <span class="act danger" title="删除（保存时生效，可撤销）" @click.stop="deleteFile(data)">✕</span>
            </span>
            <span v-else-if="!data.isDir && data.deleted" class="node-actions" @click.stop @mousedown.stop>
              <span class="act" title="撤销删除" @click.stop="restoreFile(data)">↩</span>
            </span>
          </span>
        </template>
      </el-tree>
    </el-scrollbar>

    <!-- 新建文件：约定文件优先，再自定义 -->
    <el-dialog v-model="newFileVisible" title="新建文件" width="560px" :close-on-click-modal="false" append-to-body>
      <div class="qtitle">约定文件</div>
      <div class="qlist">
        <div
          v-for="q in QUICK_FILES" :key="q.path"
          class="qitem" :class="{ disabled: quickFileExists(q.path) }"
          @click="!quickFileExists(q.path) && createQuickFile(q)"
        >
          <code class="qpath">{{ q.path }}</code>
          <span class="qdesc">{{ q.desc }}</span>
          <el-tag v-if="quickFileExists(q.path)" size="small" type="info" effect="plain">已有</el-tag>
        </div>
      </div>
      <div class="qtitle" style="margin-top: 14px">自定义路径</div>
      <el-input v-model="newFileCustom" placeholder="相对 chart 根，如 files/app.conf" @keyup.enter="createCustomFile" />
      <template #footer>
        <el-button @click="newFileVisible = false">取消</el-button>
        <el-button type="primary" @click="createCustomFile">创建</el-button>
      </template>
    </el-dialog>

    <!-- 新建目录：约定目录优先，再自定义 -->
    <el-dialog v-model="newDirVisible" title="新建目录" width="560px" :close-on-click-modal="false" append-to-body>
      <div class="qtitle">约定目录</div>
      <div class="qlist">
        <div
          v-for="q in QUICK_DIRS" :key="q.path"
          class="qitem" :class="{ disabled: dirExists(q.path) }"
          @click="!dirExists(q.path) && addDir(q.path)"
        >
          <code class="qpath">{{ q.path }}/</code>
          <span class="qdesc">{{ q.desc }}</span>
          <el-tag v-if="dirExists(q.path)" size="small" type="info" effect="plain">已有</el-tag>
        </div>
      </div>
      <div class="qtitle" style="margin-top: 14px">自定义目录</div>
      <el-input v-model="newDirCustom" placeholder="目录名，如 scripts" @keyup.enter="addCustomDir" />
      <template #footer>
        <el-button @click="newDirVisible = false">取消</el-button>
        <el-button type="primary" @click="addCustomDir">创建</el-button>
      </template>
    </el-dialog>

    <!-- 右键菜单 -->
    <teleport to="body">
      <div v-if="ctxMenu.visible" class="ctxmenu" :style="{ left: ctxMenu.x + 'px', top: ctxMenu.y + 'px' }" @click.stop>
        <div v-if="ctxIs('file')" class="ctx-item" @click="emit('open', ctxMenu.node!.path); closeCtx()">打开</div>
        <div v-if="ctxIs('file') && !isReserved(ctxMenu.node!.path)" class="ctx-item" @click="renameFile(ctxMenu.node!); closeCtx()">重命名 / 移动</div>
        <div v-if="ctxIs('file') && !isReserved(ctxMenu.node!.path)" class="ctx-item danger" @click="deleteFile(ctxMenu.node!); closeCtx()">删除</div>
        <div v-if="ctxIs('deleted')" class="ctx-item" @click="restoreFile(ctxMenu.node!); closeCtx()">撤销删除</div>
        <div v-if="ctxIs('dir')" class="ctx-item" @click="openNewFile(targetDir()); closeCtx()">在此新建文件</div>
        <div v-if="ctxIs('dir')" class="ctx-item" @click="newPhase(); closeCtx()">在此新建相位</div>
      </div>
    </teleport>
  </div>
</template>

<style scoped>
.file-tree {
  display: flex;
  flex-direction: column;
  height: 100%;
  background: #fafbfd;
}
.tree-tools {
  display: flex;
  gap: 4px;
  padding: 6px 8px;
  border-bottom: 1px solid #ebeef5;
}
.tree-scroll { flex: 1; }
.node {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 13px;
  overflow: hidden;
  flex: 1;
  min-width: 0;
}
.nlabel {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.node-actions {
  visibility: hidden;
  display: inline-flex;
  gap: 2px;
  flex: none;
}
:deep(.el-tree-node__content:hover) .node-actions,
.node:focus-within .node-actions {
  visibility: visible;
}
.act {
  cursor: pointer;
  font-size: 12px;
  color: #64748b;
  padding: 0 3px;
  border-radius: 3px;
  line-height: 16px;
}
.act:hover {
  background: #e2e8f0;
  color: #1d4ed8;
}
.act.danger:hover {
  background: #fee2e2;
  color: #dc2626;
}
.node.deleted .nlabel { text-decoration: line-through; color: #c0c4cc; }
.node.binary .nlabel { color: #909399; }
.fico {
  font-family: ui-monospace, Menlo, monospace;
  font-size: 10px;
  color: #94a3b8;
  background: #f1f5f9;
  border-radius: 3px;
  padding: 0 3px;
  min-width: 14px;
  text-align: center;
}
.fico.core { color: #d97706; background: #fef3c7; }
.tag { font-size: 10px; border-radius: 3px; padding: 0 3px; }
.tag.del { background: #fee2e2; color: #dc2626; }
.tag.bin { background: #f1f5f9; color: #94a3b8; }
.ptag.err { color: #dc2626; font-size: 10px; }
.ptag.warn { color: #d97706; font-size: 10px; }
.qtitle {
  font-size: 12px;
  color: #909399;
  margin-bottom: 6px;
}
.qlist {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.qitem {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  border: 1px solid #ebeef5;
  border-radius: 6px;
  cursor: pointer;
}
.qitem:hover:not(.disabled) {
  border-color: #2563eb;
  background: #f5f8ff;
}
.qitem.disabled {
  cursor: not-allowed;
  opacity: 0.55;
}
.qpath {
  font-family: ui-monospace, Menlo, monospace;
  font-size: 12px;
  color: #1d4ed8;
  flex: none;
  min-width: 170px;
}
.qdesc {
  flex: 1;
  font-size: 12px;
  color: #64748b;
}
.ctxmenu {
  position: fixed;
  z-index: 3000;
  background: #fff;
  border: 1px solid #e4e7ed;
  border-radius: 6px;
  box-shadow: 0 4px 14px rgba(0, 0, 0, 0.12);
  padding: 4px;
  min-width: 150px;
}
.ctx-item {
  padding: 6px 12px;
  font-size: 13px;
  border-radius: 4px;
  cursor: pointer;
}
.ctx-item:hover { background: #f3f4f6; }
.ctx-item.danger { color: #dc2626; }
</style>
