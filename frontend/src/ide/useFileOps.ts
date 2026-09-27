// 文件操作与撤销栈（自 AppIDE 抽取）：树上新建/重命名/删除/撤销删除在
// fs 落地后同步 model 与标签页；每次操作入栈，焦点不在编辑器时的
// Ctrl+Z 逆序回滚。撤销重命名失败（旧路径被占用）时操作保留在栈里可重试。

import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import type { ChartFS } from './fs'
import type { useEditorTabs } from './useEditorTabs'

// 树操作记录（撤销栈条目）：kind 与 FileTree 的 @op 事件一致；edit 携带
// 改写前的内容快照 prevContent（撤销时按快照还原）
export interface FileOp { kind: string; path: string; to?: string; prevContent?: string }

export function useFileOps(opts: {
  fs: ChartFS
  // model/tab 生命周期（useEditorTabs 的返回值）
  tabsApi: ReturnType<typeof useEditorTabs>
  // 操作落地后的内容变化联动（stashed 失效/域刷新/自动暂存），调用方组装
  onMutate: () => void
}) {
  const { fs, tabsApi, onMutate } = opts
  const { tabs, active, models, ensureModel, dropModel, closeTab } = tabsApi

  const fileOps = ref<FileOp[]>([])

  function onTreeOp(ev: { kind: string; path: string; to?: string }) {
    if (ev.kind === 'create') {
      ensureModel(ev.path)
    } else if (ev.kind === 'edit') {
      // chart.yaml 被树操作（相位声明）改写：同步 model
      const m = models.get(ev.path)
      const f = fs.get(ev.path)
      if (m && f && m.getValue() !== f.content) m.setValue(f.content)
    } else if (ev.kind === 'rename') {
      const from = ev.path, to = ev.to!
      dropModel(from)
      const f = fs.get(to)
      const i = tabs.value.indexOf(from)
      if (i >= 0) tabs.value.splice(i, 1, to)
      if (f) {
        const nm = ensureModel(to)
        if (nm) nm.setValue(f.content)
      }
      if (active.value === from) active.value = to
      if (f && !tabs.value.includes(to)) tabs.value.push(to)
      ElMessage.success(`已重命名 ${from} → ${to}`)
    } else if (ev.kind === 'delete') {
      dropModel(ev.path)
      closeTab(ev.path)
    } else if (ev.kind === 'restore') {
      ensureModel(ev.path)
    }
    fileOps.value.push(ev as FileOp)
    onMutate()
  }

  // 文件操作撤销（焦点不在编辑器时 Ctrl+Z）
  function undoFileOp() {
    const op = fileOps.value.pop()
    if (!op) return
    if (op.kind === 'create') {
      fs.remove(op.path)
      dropModel(op.path)
      closeTab(op.path)
    } else if (op.kind === 'delete') {
      fs.restore(op.path)
      ensureModel(op.path)
    } else if (op.kind === 'rename') {
      const err = fs.rename(op.to!, op.path)
      if (err) {
        fileOps.value.push(op) // 撤销失败（如旧路径被占用）：操作保留可重试
        ElMessage.warning(`撤销重命名失败：${err}`)
        return
      }
      onTreeOp({ kind: 'rename', path: op.to!, to: op.path })
      fileOps.value.pop() // 撤销不入栈
    } else if (op.kind === 'edit') {
      // 树操作对文件的程序化改写（相位声明写 chart.yaml）：按快照还原
      if (op.prevContent !== undefined) {
        const f = fs.get(op.path)
        if (f) {
          f.content = op.prevContent
          const m = models.get(op.path)
          if (m && m.getValue() !== op.prevContent) m.setValue(op.prevContent)
        }
      }
    }
    onMutate()
  }

  function onDocKeydown(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z') {
      const t = e.target as HTMLElement
      if (t.closest('.monaco-editor') || t.closest('input,textarea,[contenteditable]')) return
      if (!fileOps.value.length) return
      e.preventDefault()
      undoFileOp()
    }
  }

  return { fileOps, onTreeOp, undoFileOp, onDocKeydown }
}
