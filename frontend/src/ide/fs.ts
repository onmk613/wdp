// IDE 内存文件系统：整个编辑器的单一状态源。所有内容（含 chart.yaml/
// values.yaml/deploy.yaml 三件套）都是纯文本文件——没有结构化重写层，
// 注释/格式/未知键逐字保真。
//
// 保存/校验/暂存都从这份快照序列化（同一份代码），杜绝"暂存能存、
// 保存拼不出"的漂移。底本文件未改动的不回传（后端在底本副本上就地
// 修改），二进制文件只读随包保留。

import { reactive } from 'vue'
import type { AppSpec, SpecSaveBody } from '../api'

export interface FSFile {
  path: string
  content: string // 当前内容（binary 恒 ''，不可编辑）
  original: string // 底本内容；新建文件为 ''（与 content 区分出新增）
  binary: boolean
  size: number // binary 的底本大小
  deleted: boolean // 标记删除（条目保留便于撤销/保存时回传 delete_files）
  fromBase: boolean // 底本存在（编辑模式；未改动且未删除时无需回传）
}

export interface DraftPayload {
  name: string
  version: string
  description: string
  base_version: string
  pools: string[]
  groups: string[]
  labels: string
  files: { path: string; content?: string; binary?: boolean; size?: number }[]
  deleted_files: string[]
  open_tabs: string[]
  active_tab: string
  saved_at: string
  // auto = 编辑中自动暂存（异常中断后靠横幅恢复）
  // deliberate = 离开时用户明确选择暂存（重进时底本一致则静默恢复）
  mode?: 'auto' | 'deliberate'
}

// chart 结构保留文件：不允许重命名/删除（删了必然保存失败，UI 层直接拦）
export const RESERVED = new Set(['chart.yaml', 'values.yaml', 'deploy.yaml'])

export function isReserved(path: string): boolean {
  return RESERVED.has(path)
}

// pathOk 路径合法性（与后端 secureJoin 防御一致：禁 ..、绝对路径、空段、反斜杠）
export function pathOk(p: string): boolean {
  if (!p || p.includes('\\') || p.startsWith('/') || p.includes('..')) return false
  const segs = p.split('/')
  if (segs.some((s) => !s.trim() || s === '.')) return false
  if (segs.some((s) => /[:*?"<>|]/.test(s))) return false
  return true
}

// 单文件文本上限（与后端 specTextLimit 一致）：超过的后端读取时按二进制
// 列出（只读不可编辑）。上传侧用同一阈值拦住，避免「能传能存、重开变只读」。
export const SPEC_TEXT_LIMIT = 512 * 1024

export class ChartFS {
  files = reactive(new Map<string, FSFile>())
  name = ''
  description = ''
  pools: string[] = []
  groups: string[] = []
  labels = '{}'
  baseVersion = ''

  loadFromSpec(spec: AppSpec, baseVersion: string) {
    this.files.clear()
    // spec 把 values/deploy 拆成独立字段，这里合并回文件视角
    const add = (path: string, content: string, binary = false, size = 0) => {
      this.files.set(path, reactive({
        path, content: binary ? '' : content, original: binary ? '' : content,
        binary, size, deleted: false, fromBase: true,
      }))
    }
    add('deploy.yaml', spec.deploy_yaml || '')
    add('values.yaml', spec.values_yaml || '{}\n')
    for (const f of spec.files || []) {
      if (f.path === 'deploy.yaml' || f.path === 'values.yaml') continue // 已合并
      if (f.binary) add(f.path, '', true, f.size)
      else add(f.path, f.content || '')
    }
    this.name = spec.name
    this.description = spec.description || ''
    this.pools = spec.pools || []
    this.groups = spec.groups || []
    this.labels = spec.labels || '{}'
    this.baseVersion = baseVersion
  }

  loadFromScaffold(name: string, version: string, desc: string) {
    this.files.clear()
    // original 取脚手架内容：新建模式以脚手架为「上次」基线，未编辑时
    // isDirty() 为 false（离开不再弹未保存确认）。fromBase 仍为 false，
    // toSaveBody 照样全量回传（新建本就要落全部文件）。
    const mk = (path: string, content: string) => {
      this.files.set(path, reactive({
        path, content, original: content, binary: false, size: content.length,
        deleted: false, fromBase: false,
      }))
    }
    mk('chart.yaml', scaffoldChart(name, version, desc))
    mk('values.yaml', scaffoldValues())
    mk('deploy.yaml', scaffoldDeploy(name))
    this.name = name
    this.description = desc
    this.pools = []
    this.groups = []
    this.labels = '{}'
    this.baseVersion = ''
  }

  loadFromDraft(d: DraftPayload) {
    this.files.clear()
    for (const f of d.files) {
      this.files.set(f.path, reactive({
        path: f.path,
        content: f.binary ? '' : (f.content ?? ''),
        original: '', // 草稿恢复后一切按"已改动"处理：保存时全量回传
        binary: !!f.binary,
        size: f.size || 0,
        deleted: false,
        fromBase: false,
      }))
    }
    // 草稿快照含全部文件与删除标记，底本无需补差集。
    for (const p of d.deleted_files) {
      const f = this.files.get(p)
      if (f) {
        f.deleted = true
        f.fromBase = true
      }
    }
    this.name = d.name
    this.description = d.description
    this.pools = d.pools || []
    this.groups = d.groups || []
    this.labels = d.labels || '{}'
    this.baseVersion = d.base_version || ''
  }

  get(path: string): FSFile | undefined {
    return this.files.get(path)
  }

  // 文本文件（未删除、非二进制），排序稳定
  textFiles(): FSFile[] {
    const out: FSFile[] = []
    for (const f of this.files.values()) {
      if (!f.binary && !f.deleted) out.push(f)
    }
    return out.sort((a, b) => a.path.localeCompare(b.path))
  }

  allFiles(): FSFile[] {
    return [...this.files.values()].sort((a, b) => a.path.localeCompare(b.path))
  }

  create(path: string, content: string): string | null {
    path = path.trim()
    if (!pathOk(path)) return '路径不合法（相对 chart 根，禁 .. 与绝对路径）'
    if (this.files.has(path)) {
      const ex = this.files.get(path)!
      if (ex.deleted) {
        ex.deleted = false
        ex.content = content
        return null // 删除过的重建 = 恢复
      }
      return '文件已存在'
    }
    this.files.set(path, reactive({
      path, content, original: '', binary: false, size: content.length,
      deleted: false, fromBase: false,
    }))
    return null
  }

  remove(path: string): string | null {
    const f = this.files.get(path)
    if (!f || f.deleted) return '文件不存在'
    if (isReserved(path)) return `${path} 是 chart 必需文件，不能删除`
    f.deleted = true
    return null
  }

  restore(path: string): string | null {
    const f = this.files.get(path)
    if (!f || !f.deleted) return '文件未删除'
    f.deleted = false
    return null
  }

  rename(from: string, to: string): string | null {
    to = to.trim()
    const f = this.files.get(from)
    if (!f || f.deleted) return '文件不存在'
    if (isReserved(from)) return `${from} 是 chart 必需文件，不能重命名`
    if (!pathOk(to)) return '目标路径不合法'
    if (this.files.has(to)) {
      const ex = this.files.get(to)!
      if (!ex.deleted) return '目标路径已存在'
      this.files.delete(to) // 覆盖先前删除的占位
    }
    // 底本存在的文件：旧路径登记删除占位（保存时进 delete_files）。此前
    // 直接把旧条目移出 Map，保存请求既无新路径也无旧路径删除——重命名
    // 静默丢失，甚至新旧并存（复制而非移动）。占位保留原内容，树上
    // 「撤销删除」即找回底本文件（相当于撤销重命名）。
    // 本会话新建的文件旧路径本就不在包里，直接移出即可。
    if (f.fromBase) {
      this.files.set(from, reactive({ ...f, path: from, deleted: true }))
    } else {
      this.files.delete(from)
    }
    // 新路径按「待回传」处理（original=''）：路径本身就是改动，内容必须
    // 在新路径下重新落盘；否则 toSaveBody 视其与底本一致而不回传，
    // 旧删新增变成只删不增，文件整体丢失
    const nf = reactive({ ...f, path: to, ...(f.fromBase ? { original: '' } : {}) })
    this.files.set(to, nf)
    return null
  }

  isDirty(): boolean {
    for (const f of this.files.values()) {
      if (f.deleted) return true
      if (!f.binary && f.content !== f.original) return true
    }
    return false
  }

  dirtyCount(): number {
    let n = 0
    for (const f of this.files.values()) {
      if (f.deleted || (!f.binary && f.content !== f.original)) n++
    }
    return n
  }

  // toSaveBody 序列化保存/校验请求：改动的文本文件 + 删除清单。
  // chart.yaml 的版本号由调用方在保存前直接 patch 进 f.content（见
  // AppIDE 的保存流程），toSaveBody 只做序列化。
  toSaveBody(version: string): SpecSaveBody {
    const files: SpecSaveBody['files'] = []
    for (const f of this.files.values()) {
      if (f.binary || f.deleted) continue
      const content = f.content
      const changed = !f.fromBase || content !== f.original || f.path === 'chart.yaml'
      if (!changed) continue
      files.push({ path: f.path, content, size: content.length })
    }
    const deleteFiles: string[] = []
    for (const f of this.files.values()) {
      if (f.deleted) deleteFiles.push(f.path)
    }
    return {
      name: this.name,
      version,
      description: this.description,
      files,
      delete_files: deleteFiles,
      base_version: this.baseVersion,
      pools: this.pools,
      groups: this.groups,
      labels: this.labels,
    }
  }

  // toDraftPayload 草稿快照：全量文本文件 + UI 状态（恢复时整体重建）
  toDraftPayload(openTabs: string[], activeTab: string, mode: 'auto' | 'deliberate' = 'auto'): DraftPayload {
    const files: DraftPayload['files'] = []
    const deleted: string[] = []
    for (const f of this.files.values()) {
      if (f.deleted) {
        deleted.push(f.path)
        // 已删除文件同样快照内容/二进制标记（除路径外）：恢复草稿后
        // "撤销删除"才能拿回完整文件。此前只记路径，loadFromDraft 找不
        // 到内容会误登记成二进制占位——内容丢失且不可再编辑
        files.push(f.binary
          ? { path: f.path, binary: true, size: f.size }
          : { path: f.path, content: f.content })
        continue
      }
      files.push(f.binary
        ? { path: f.path, binary: true, size: f.size }
        : { path: f.path, content: f.content })
    }
    return {
      name: this.name,
      version: '',
      description: this.description,
      base_version: this.baseVersion,
      pools: this.pools,
      groups: this.groups,
      labels: this.labels,
      files,
      deleted_files: deleted,
      open_tabs: openTabs,
      active_tab: activeTab,
      saved_at: new Date().toISOString(),
      mode,
    }
  }

  // markSaved 保存成功后重置基线：全部按底本、删除清空
  markSaved(baseVersion: string) {
    for (const [p, f] of [...this.files.entries()]) {
      if (f.deleted) {
        this.files.delete(p)
        continue
      }
      f.original = f.content
      f.fromBase = true
    }
    this.baseVersion = baseVersion
  }
}

// ---- 新建应用脚手架 ----

export function scaffoldChart(name: string, version: string, desc: string): string {
  return [
    '# chart 元数据（必需）。name/version 与保存的版本号对账，必须一致',
    `name: ${name}`,
    `version: ${version}`,
    ...(desc ? [`description: ${desc}`] : []),
    '# required: [app.port]   # 必须由调用方提供的 values 点路径（可选）',
    '',
  ].join('\n')
}

export function scaffoldValues(): string {
  return [
    '# values.yaml —— 模板变量默认值（可选；模板里 {{ .app.port }} 引用）',
    '# 新增变量后在模板文件里输入 . 即可补全',
    '{}',
    '',
  ].join('\n')
}

export function scaffoldDeploy(appName: string): string {
  return [
    '# deploy.yaml —— 部署相位（必需）。直接写任务即可：',
    '#   · web 执行：目标主机由执行时的选择器决定，无需关心 hosts',
    `#   · CLI 独立执行：默认打与应用同名的主机组（${appName}）`,
    '# 需要一个相位内按主机组分工时，再用 play 形态：',
    '#   - name: web 部分',
    '#     hosts: webservers',
    '#     tasks: [...]',
    "- name: 第一个任务（示例，改掉我）",
    "  shell: 'echo hello'",
    '',
  ].join('\n')
}

// patchChartYAMLVersion 把顶层 version: 行替换为新版本号（保留注释与
// 其余内容逐字不动）。找不到顶层 version 行时在 name 行后插入。
export function patchChartYAMLVersion(content: string, newVersion: string): string {
  const lines = content.split('\n')
  let vi = -1, ni = -1
  for (let i = 0; i < lines.length; i++) {
    if (/^version:\s/.test(lines[i]) || lines[i] === 'version:') { vi = i; break }
    if (/^name:\s/.test(lines[i])) ni = i
  }
  if (vi >= 0) {
    lines[vi] = lines[vi].replace(/^version:[^\n]*/, `version: ${newVersion}`)
    return lines.join('\n')
  }
  const at = ni >= 0 ? ni + 1 : 0
  lines.splice(at, 0, `version: ${newVersion}`)
  return lines.join('\n')
}

// chartYAMLVersion 解析顶层 version（容错：解析失败返回 ''）
export function chartYAMLVersion(content: string): string {
  const m = content.match(/^version:[ \t]+(.+)$/m)
  return m ? m[1].trim().replace(/^["']|["']$/g, '') : ''
}

// yamlScalarSafe 描述等自由文本落到 yaml 行时按需加双引号：含冒号+空格/
// #注释起始等字符的裸标量会破坏 chart.yaml 解析。JSON 字符串转义是合法
// 的 YAML 双引号标量，直接复用。
function yamlScalarSafe(v: string): string {
  return /[:#]/.test(v) || v !== v.trim() ? JSON.stringify(v) : v
}

// patchChartYAMLDescription 把顶层 description: 行替换为新描述（无该行时
// 追加）。此前保存只在「没有 description 行」时追加——已有该行时改描述
// 只更新库不更新文件，两边口径漂移，下次进编辑器又带出旧描述。
export function patchChartYAMLDescription(content: string, desc: string): string {
  const safe = yamlScalarSafe(desc)
  const lines = content.split('\n')
  for (let i = 0; i < lines.length; i++) {
    if (/^description:[ \t]*/.test(lines[i]) || lines[i] === 'description:') {
      lines[i] = lines[i].replace(/^description:[^\n]*/, `description: ${safe}`)
      return lines.join('\n')
    }
  }
  return `${content.trimEnd()}\ndescription: ${safe}\n`
}

export function chartYAMLDescription(content: string): string {
  const m = content.match(/^description:[ \t]+(.+)$/m)
  return m ? m[1].trim().replace(/^["']|["']$/g, '') : ''
}

// bumpVersion/nextVersion 版本号推进
export function bumpVersion(v: string): string {
  const m = v.match(/^(.*?)(\d+)$/)
  if (!m) return v ? v + '.1' : '1.0.0'
  return m[1] + String(parseInt(m[2], 10) + 1)
}

export function nextVersion(base: string, taken: string[]): string {
  if (!base) return '1.0.0'
  let v = bumpVersion(base)
  for (let i = 0; i < 200 && taken.includes(v); i++) v = bumpVersion(v)
  return v
}
