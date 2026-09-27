// Monaco 补全 providers：
//   1) YAML 键补全——play 键 / 任务模块名（c → command/copy/chart…）/ 任务
//      控制键（when/loop/until…）/ 模块参数（模块确定后）
//   2) YAML 值补全——src/include 文件路径、chart 子包名、tasks_from 相位、
//      hosts 主机/组、notify handler 名
//   3) {{ }} 模板补全——values 变量（跨文件实时）、helpers define 名、
//      register 变量、内置变量、sprig 白名单函数
// 数据源全部来自后端单一事实来源（/api/modules、/api/schema）+ 内存 FS，
// provider 闭包持有 getter（内容变化时上下文自身更新，无需重注册）。

import type * as Monaco from 'monaco-editor'
import type { MonacoNs } from './monaco'
import type { ModuleMeta, SchemaMeta } from '../api'
import type { ChartFS } from './fs'
import { yamlCtx, inTemplateExpr, templateExprKind } from './ctx'
import { controlKeys, playKeysSet } from './keys'
import { taskChartRef } from './chartref'
import type { VarDomain } from './vars'
import { subchartNames, phaseNames, handlerNames, resolvePathPrefix } from './vars'

export interface CompleteDeps {
  fs: ChartFS
  meta: () => SchemaMeta | null
  modules: () => ModuleMeta[] | null
  domain: () => VarDomain | null
  hostGroups: () => { hosts: string[]; groups: string[] } | null
  // 根级形态缺省：chart 相位文件默认裸任务、playbook 编辑器默认 play
  rootDefaultIsTask?: () => boolean
}

// 模块键写法（写在模块位置，不进控制键候选）
const MODULE_POSITION = new Set(['chart', 'include'])

function docMarkdown(m: ModuleMeta): string {
  const rows = (m.params || [])
    .map((p) => `| \`${p.name}\` | ${p.type} | ${p.default || '-'} | ${p.enum?.length ? p.enum.join(' / ') + ' — ' : ''}${p.desc} |`)
    .join('\n')
  return [
    `**${m.name}** — ${m.desc}`,
    m.rollback ? `\n回滚能力：${['无', '部分', '全量'][m.rollback] || '-'}` : '',
    rows ? `\n\n| 参数 | 类型 | 默认 | 说明 |\n| --- | --- | --- | --- |\n${rows}` : '',
    m.example ? `\n\n示例：\n\`\`\`yaml\n${m.example.trim()}\n\`\`\`` : '',
  ].join('')
}

function fieldDoc(sec: SchemaMeta['task'][number] | SchemaMeta['play'][number], name: string): string {
  const f = sec.Fields.find((x) => x.Name === name)
  return f ? `${f.Desc}${f.Default && f.Default !== '-' ? `（默认 \`${f.Default}\`，类型 ${f.Type}）` : `（类型 ${f.Type}）`}` : ''
}

// 常用模块置顶
const COMMON_MODULES = ['shell', 'copy', 'template', 'file', 'service', 'package', 'user', 'group', 'systemd', 'stat', 'set_fact', 'wait_for', 'lineinfile']

// 模板函数说明（白名单 = 后端 render.AllowlistFuncs，docs/09 全集的投影；
// 未列出的白名单函数落回按类别的兜底说明）。新手在补全弹窗里就能看到
// 每个函数干什么、怎么用。
const FUNC_DOCS: Record<string, string> = {
  // 字典/列表
  dict: '构造字典：`{{ dict "k1" 1 "k2" 2 }}` → {k1:1, k2:2}',
  list: '构造列表：`{{ list 1 2 3 }}` → [1,2,3]',
  concat: '合并多个列表：`{{ concat $a $b }}`',
  append: '尾部追加：`{{ append $list 4 }}`',
  prepend: '头部插入：`{{ prepend $list 0 }}`',
  first: '取列表第一个元素；空列表返回空字符串',
  last: '取列表最后一个元素',
  rest: '取列表除第一个外的其余元素',
  initial: '取列表除最后一个外的其余元素',
  reverse: '反转列表',
  uniq: '去重（保持顺序）',
  without: '剔除指定元素：`{{ without $list 2 }}`',
  has: '包含判定：`{{ has $list 2 }}` → true/false',
  compact: '去掉列表中的空值元素',
  dig: '安全取嵌套值：`{{ dig "a" "b" "默认" $dict }}`（链上缺键给默认值，不报错）',
  keys: '取字典全部键（排序后）',
  pick: '按键挑选子字典：`{{ pick $dict "k1" "k2" }}`',
  omit: '按键剔除子字典（pick 的反面）',
  merge: '深合并字典：`{{ merge $dst $src }}`（src 覆盖 dst）',
  values: '取字典全部值（按键序）',
  // 字符串
  trim: '去掉两端空白',
  trimAll: '去掉两端指定字符集：`{{ trimAll "-" "--x--" }}` → x',
  trimPrefix: '去掉指定前缀',
  trimSuffix: '去掉指定后缀',
  upper: '转大写；lower 转小写',
  lower: '转小写',
  title: '首字母大写（Title Case）',
  untitle: 'title 的反面（整串小写化）',
  repeat: '重复 N 次：`{{ repeat 3 "ab" }}` → ababab',
  substr: '子串：`{{ substr 0 5 "hello world" }}`（起止下标）',
  trunc: '截断到 N 字符：`{{ trunc 5 $s }}`',
  abbrev: '超长时中间省略（保头尾）',
  initials: '取各单词首字母',
  randAlpha: '随机字母串：`{{ randAlpha 8 }}`（常用生成随机口令/后缀）',
  randAlphaNum: '随机字母数字串：`{{ randAlphaNum 8 }}`',
  randNumeric: '随机数字串：`{{ randNumeric 6 }}`',
  wrap: '按宽度折行：`{{ wrap 80 $text }}`',
  contains: '包含子串判定：`{{ contains "ell" "hello" }}` → true',
  hasPrefix: '前缀判定；hasSuffix 后缀判定',
  hasSuffix: '后缀判定',
  quote: '加双引号（多值各加）；squote 单引号——生成安全 shell 参数常用',
  squote: "加单引号（多值各加）",
  cat: '空格拼接多个值：`{{ cat "a" 1 "b" }}` → "a 1 b"',
  indent: '每行加 N 空格缩进（首行不加）；nindent = 换行 + indent（嵌入 yaml 片段常用）',
  nindent: '换行并缩进 N 空格（yaml 内嵌块必用：`{{ tpl ... | nindent 4 }}`）',
  replace: '替换全部：`{{ replace "a" "b" "aaa" }}` → bbb',
  plural: '单复数：`{{ plural "item" $n }}`（1 item / 2 items）',
  sha1sum: 'SHA-1 摘要（十六进制）；sha256sum SHA-256',
  sha256sum: 'SHA-256 摘要（十六进制）——比对文件内容是否变化常用',
  adler32sum: 'Adler-32 校验和',
  toString: '任意值转字符串（数字 → "80"）',
  atoi: '字符串转 int（失败给 0）；int/int64/float64 同类',
  int: '转 int',
  int64: '转 int64',
  float64: '转 float64',
  seq: '生成序列：`{{ seq 1 5 }}` → 1 2 3 4 5',
  toDecimal: '八进制字符串转十进制：`{{ "0755" | toDecimal }}` → 493',
  // 正则
  regexMatch: '匹配判定：`{{ regexMatch "^a." $s }}` → true/false（Go 语法 RE2）',
  regexFind: '取第一个匹配子串；regexFindAll 取全部',
  regexFindAll: '取全部匹配：`{{ regexFindAll "\\d+" $s -1 }}`',
  regexReplaceAll: '正则替换（$1 引用分组）',
  regexReplaceAllLiteral: '正则替换（$1 按字面，不展开分组）',
  regexSplit: '正则切分：`{{ regexSplit "," $s -1 }}`',
  regexQuoteMeta: '转义正则元字符（把用户输入安全当字面量用）',
  // 类型与判定
  toJson: '序列化为 JSON 字符串——写入配置文件/比对结构常用',
  fromJson: '解析 JSON 字符串为对象',
  toPrettyJson: '带缩进的美化 JSON',
  ternary: '三目：`{{ ternary "yes" "no" $cond }}`',
  default: '空值兜底（最常用）：`{{ .app.port | default "8080" }}`——值为空/缺省时用给定默认',
  empty: '空值判定（空串/0/nil/空集合为 true）',
  coalesce: '取第一个非空值：`{{ coalesce $a $b "fallback" }}`',
  all: '全部为真（and）；any 任一为真（or）',
  any: '任一为真',
  kindOf: '值的种类（"string"/"int"/"slice"…）；typeOf 带 Go 类型名',
  typeOf: '完整 Go 类型名',
  kindIs: '种类判定：`{{ kindIs "int" $v }}`',
  typeIs: '类型判定（含类型名）',
  // 数学
  add: '加；sub 减；mul 乘；div 除；mod 取余；add1 自增',
  sub: '减法',
  mul: '乘法',
  div: '除法',
  mod: '取余',
  max: '取大；min 取小',
  min: '取小',
  ceil: '向上取整；floor 向下取整；round 保留 N 位小数',
  floor: '向下取整',
  round: '四舍五入到 N 位：`{{ round 3.14159 2 }}` → 3.14',
  add1: '自增 1（循环计数常用）',
  // 日期
  now: '当前时间（time.Time）',
  date: '格式化时间：`{{ now | date "2006-01-02" }}`（Go 布局语法）',
  dateInZone: '按指定时区格式化',
  dateModify: '时间偏移：`{{ now | dateModify "-24h" }}`',
  duration: '时长解析：`{{ duration "95m" }}` → 1h35m0s',
  unixEpoch: 'Unix 秒：`{{ now | unixEpoch }}`',
  htmlDate: 'HTML date 控件格式（YYYY-MM-DD）',
  toDate: '字符串解析为时间：`{{ toDate "2006-01-02" "2024-01-01" }}`',
  // 编码
  b64enc: 'Base64 编码：`{{ $s | b64enc }}`（写 secret/内联数据常用）；b64dec 解码',
  b64dec: 'Base64 解码',
  b32enc: 'Base32 编码',
  b32dec: 'Base32 解码',
  // 流程与其他
  fail: '渲染即失败（配置校验兜底）：`{{ fail "app.port 必须提供" }}`',
  uuidv4: '随机 UUID v4',
  semver: '解析语义化版本对象（.major/.minor/.patch）',
  semverCompare: '版本区间判定：`{{ semverCompare ">=1.2.0" $v }}`',
}
// 兜底：按函数名前缀给类别级说明（白名单新增而文档未跟随时）
const FUNC_DOCS_FALLBACK: [RegExp, string][] = [
  [/^regex/, '正则函数（Go RE2 语法），全集见文档「模板函数速查表」'],
  [/^rand/, '随机值生成（每次渲染都不同——固定值需求勿用）'],
  [/^date|^now|^unix|^html|^to|^duration/, '日期时间函数，Go 布局语法（2006-01-02 15:04:05）'],
  [/./, 'sprig 白名单模板函数；用法见文档「模板函数速查表」'],
]
function funcDoc(name: string): string {
  return FUNC_DOCS[name] || FUNC_DOCS_FALLBACK.find(([re]) => re.test(name))![1]
}

// 变量文档：按来源（values/register/循环变量/内置）给新手可用的说明
function varDoc(path: string, detail: string | undefined, hasChildren: boolean): string {
  const example = `{{ .${path} }}`
  if (detail === 'register') {
    return `任务 register: ${path} 注册的结果变量。整对象 ${example}；常用字段 ${path}.stdout（输出）、${path}.rc（退出码）、${path}.changed（是否有变更）——输入 . 可继续补全`
  }
  if (detail === '循环变量') {
    return `loop 循环的当前项（loop_control.loop_var: ${path} 自定义的名字）。循环内 ${example} 取该项的值`
  }
  if (detail === '内置变量') {
    return `引擎内置变量：${example}。无需在 values.yaml 定义`
  }
  if (path === 'item') {
    return 'loop 循环的当前项：循环内 `{{ .item }}` 取值；列表项是字典时 `{{ .item.key }}` 取子字段'
  }
  if (detail === 'values.yaml' || (!detail && hasChildren)) {
    return `values.yaml 定义的变量（可在编辑器里修改默认值）。引用写法 ${example}；常配默认值兜底：{{ .${path} | default "…" }}`
  }
  return `变量 ${path}，引用写法 ${example}`
}

// register 结果的常用字段（叶子变量后的续补全）
const RESULT_FIELDS: { name: string; doc: string }[] = [
  { name: 'stdout', doc: '标准输出（shell/command 类模块）' },
  { name: 'stderr', doc: '标准错误输出' },
  { name: 'rc', doc: '退出码（0 = 成功）' },
  { name: 'changed', doc: '模块是否产生了变更（bool）——when 条件里常用' },
  { name: 'failed', doc: '任务是否失败（bool）' },
  { name: 'skipped', doc: '任务是否被跳过（bool）' },
  { name: 'msg', doc: 'fail/assert 类模块的消息' },
  { name: 'results', doc: 'loop 循环逐项结果的列表（每项含 .item/.stdout/.rc…）' },
]

// 注册补全 provider，返回释放句柄。注意：provider 注册在 monaco 语言层
// 全局生效，不随编辑器销毁而释放——视图卸载时必须 dispose，否则路由
// 往返一次就叠一层，所有候选出现两份/多份（曾发生的孪生重复根因）。
export function registerProviders(monaco: MonacoNs, deps: CompleteDeps): Monaco.IDisposable {
  const K = monaco.languages.CompletionItemKind
  const S = monaco.languages.CompletionItemInsertTextRule.InsertAsSnippet
  const disposables: Monaco.IDisposable[] = []

  const makeItem = (
    label: string, kind: Monaco.languages.CompletionItemKind, insert: string,
    range: Monaco.IRange, opts?: { detail?: string; doc?: string; sort?: string; command?: string },
  ): Monaco.languages.CompletionItem => ({
    label, kind, insertText: insert, range,
    detail: opts?.detail,
    documentation: opts?.doc ? { value: opts.doc } : undefined,
    sortText: opts?.sort,
    insertTextRules: insert.includes('$') ? S : undefined,
  })

  // ---- YAML 键/值补全 ----
  disposables.push(monaco.languages.registerCompletionItemProvider('yaml', {
    triggerCharacters: [':', '-', ' ', '.', '"', "'"],
    provideCompletionItems(model: Monaco.editor.ITextModel, position: Monaco.Position) {
      const meta = deps.meta()
      if (!meta) return { suggestions: [] }
      const word = model.getWordUntilPosition(position)
      const range: Monaco.IRange = {
        startLineNumber: position.lineNumber, endLineNumber: position.lineNumber,
        startColumn: word.startColumn, endColumn: word.endColumn,
      }
      const ctx = yamlCtx(model.getValue(), position, controlKeys(meta), playKeysSet(meta), deps.rootDefaultIsTask ? deps.rootDefaultIsTask() : true)
      if (!ctx) return { suggestions: [] }

      if (ctx.kind === 'value') return { suggestions: valueSuggestions(deps, ctx.key, ctx.module, ctx.quoted, range, K, model, position) }

      const suggestions: Monaco.languages.CompletionItem[] = []
      const lineText = model.getLineContent(position.lineNumber)
      const dash = /^(\s*)- /.exec(lineText)
      // 多行 snippet 的后续行缩进必须写"相对行首"的深度：Monaco 插入
      // snippet 时会自动把当前行行首缩进加到每个后续行上，snippet 里再
      // 带完整缩进就会叠双层。
      //   模块键的参数必须是模块键的子节点（比键深一层，与仓库示例
      //   get_url/url 的层级一致）——与键同列会成为任务 map 的同级键，
      //   解析时被当成第二个模块键，语义就错了：
      //   dash 行（"- shell:" 键在 行首+2）：参数在 行首+4 → 相对 4
      //   非 dash 行（续行键在行首）：参数在 行首+2 → 相对 2
      const paramRelIndent = dash ? '    ' : '  '
      // 列表值项（参数键的 "- " 项）：比该键再深 2
      const childIndent = '  '

      if (ctx.level === 'play') {
        for (const sec of meta.play) {
          for (const f of sec.Fields) {
            if (ctx.existing.has(f.Name)) continue
            suggestions.push(makeItem(f.Name, K.Field, `${f.Name}: `, range, {
              detail: `play · ${f.Type}`,
              doc: fieldDoc(sec, f.Name), sort: '1' + f.Name,
            }))
          }
        }
        suggestions.push(makeItem('tasks', K.Field, 'tasks:\n' + childIndent + '- ', range, {
          detail: 'play · 任务列表', doc: '主任务列表（按序执行）', sort: '0tasks',
        }))
        return { suggestions }
      }

      // task 级
      const mods = deps.modules() || []
      if (ctx.level === 'task' && !ctx.module) {
        for (const m of mods) {
          if (m.name === 'chart') continue // 伪模块条目：由下方带骨架的显式项提供，避免重复候选

          const params = (m.params || []).filter((p) => p.name !== '(free-form)' && p.name !== '')
          let snippet = `${m.name}: $0`
          if (params.length) {
            const ph = params.slice(0, 4).map((p, i) => `${paramRelIndent}${p.name}: ` + '${' + (i + 1) + '}').join('\n')
            snippet = `${m.name}:\n${ph}\n${paramRelIndent}$0`
          }
          suggestions.push(makeItem(m.name, K.Module, snippet, range, {
            detail: `模块 · ${m.desc}`,
            doc: docMarkdown(m),
            sort: (COMMON_MODULES.includes(m.name) ? '0' : '1') + m.name,
          }))
        }
        suggestions.push(makeItem('chart', K.Module, 'chart:\n' + paramRelIndent + 'name: $0', range, {
          detail: '引用 chart（map 唯一形态）', sort: '0chart',
          doc: '`chart: {name: <引用名>, values/values_from/hosts/phase}`：引用 chart 展开为任务序列，可与普通任务混排。chart 内引用 charts/ 子 chart；裸 playbook 引用同级 charts/ 目录',
        }))
        suggestions.push(makeItem('include', K.Module, 'include: ', range, {
          detail: '引入任务片段', sort: '1include',
          doc: '`include: tasks/x.yaml`：引入任务片段，Load 期静态展开',
        }))
      }

      // 控制键（task 级；moduleArgs 级不给）
      if (ctx.level === 'task') {
        for (const sec of meta.task) {
          for (const f of sec.Fields) {
            if (MODULE_POSITION.has(f.Name)) continue
            if (ctx.existing.has(f.Name)) continue
            if (['block', 'rescue', 'always'].includes(f.Name)) continue
            suggestions.push(makeItem(f.Name, K.Field, `${f.Name}: `, range, {
              detail: `控制键 · ${f.Type}`,
              doc: fieldDoc(sec, f.Name), sort: '2' + f.Name,
            }))
          }
        }
      }

      // 模块参数（模块已确定，或正处于模块参数块）。排序按后端 ParamDoc
      // 声明序（即使用顺序：copy 的 src→dest、file 的 path→state）——
      // 此前按字母排序，src 这类主参数被排到末尾，不合使用习惯
      const mod = ctx.level === 'moduleArgs' ? ctx.module : undefined
      const mmeta = mod ? mods.find((m) => m.name === mod) : undefined
      if (mmeta) {
        let idx = 0
        for (const p of mmeta.params || []) {
          idx++
          if (p.name === '(free-form)' || !p.name) continue
          if (ctx.existing.has(p.name)) continue
          const v = p.type === 'bool' ? `${p.name}: ` : p.type === 'list' ? `${p.name}:\n${childIndent}- ` : `${p.name}: `
          suggestions.push(makeItem(p.name, K.Field, v, range, {
            detail: `${mmeta.name} 参数 · ${p.type}${p.default ? ` · 默认 ${p.default}` : ''}`,
            doc: p.desc, sort: '0' + String(idx).padStart(3, '0'),
          }))
        }
      }
      return { suggestions }
    },
  }))

  // ---- {{ }} 模板补全（yaml / gotemplate / plaintext 通用） ----
  const templateProvider: Monaco.languages.CompletionItemProvider = {
    triggerCharacters: ['.', '"', '{', ' '],
    provideCompletionItems(model: Monaco.editor.ITextModel, position: Monaco.Position) {
      const domain = deps.domain()
      if (!domain) return { suggestions: [] }
      const lineBefore = model.getValueInRange({
        startLineNumber: position.lineNumber, startColumn: 1,
        endLineNumber: position.lineNumber, endColumn: position.column,
      })
      const expr = inTemplateExpr(lineBefore)
      if (expr === null) return { suggestions: [] }
      const kind = templateExprKind(expr)
      // 叶子变量接受后自动闭合 }}（用户常忘写右括号）；已有 }} 不重复。
      // 空格风格说明：Go template 中 {{.x}} 与 {{ .x }} 等价，不影响执行，
      // 这里跟随用户已输入风格，只在闭合处补一个空格
      const needsClose = (): boolean =>
        !model.getLineContent(position.lineNumber).slice(position.column - 1).trimStart().startsWith('}}')
      const word = model.getWordUntilPosition(position)
      const range: Monaco.IRange = {
        startLineNumber: position.lineNumber, endLineNumber: position.lineNumber,
        startColumn: word.startColumn || position.column, endColumn: word.endColumn || position.column,
      }
      const out: Monaco.languages.CompletionItem[] = []

      if (kind.kind === 'helper') {
        const m = expr.match(/"([^"]*)$/)
        for (const h of domain.helpers) {
          if (m && !h.startsWith(m[1])) continue
          out.push(makeItem(h, K.Reference, `"${h}"`, range, {
            detail: '_helpers.tpl 定义',
            doc: '_helpers.tpl 里 `{{ define "名字" }}` 定义的命名模板。此处补全写 `include "名字" .`，把定义内容渲染到当前位置；可带管道加工：`include "app.name" . | trim`',
            sort: '0' + h,
          }))
        }
        return { suggestions: out }
      }

      if (kind.kind === 'path') {
        const res = expr.trim() === '.' || expr.trim() === ''
          ? { node: { name: '', children: domain.roots }, rest: '', chain: [] as string[] }
          : resolvePathPrefix(domain, expr)
        if (res) {
          for (const child of Object.values(res.node.children || {})) {
            if (res.rest && !child.name.startsWith(res.rest)) continue
            const leaf = !child.children || Object.keys(child.children).length === 0
            out.push(makeItem(child.name, K.Variable, child.name + (leaf && needsClose() ? ' }}' : ''), range, {
              detail: [...res.chain, child.name].join('.') + (child.children ? ' ↧' : '') + (child.detail ? ` · ${child.detail}` : ''),
              doc: varDoc([...res.chain, child.name].join('.'), child.detail, !!child.children),
              sort: '0' + child.name,
            }))
          }
          // 叶子节点的常用结果字段（register 变量的 .stdout/.rc）
          if (!Object.keys(res.node.children || {}).length) {
            for (const f of RESULT_FIELDS) {
              out.push(makeItem(f.name, K.Variable, f.name, range, { detail: '结果字段', doc: f.doc, sort: '1' + f.name }))
            }
          }
        }
        return { suggestions: out }
      }

      // token：函数 + 根变量
      for (const fn of domain.funcs) {
        out.push(makeItem(fn, K.Function, fn, range, { detail: '模板函数', doc: funcDoc(fn), sort: '1' + fn }))
      }
      for (const root of Object.values(domain.roots)) {
        const leaf = !root.children || Object.keys(root.children).length === 0
        out.push(makeItem('.' + root.name, K.Variable, '.' + root.name + (leaf && needsClose() ? ' }}' : ''), range, {
          detail: root.detail || '变量',
          doc: varDoc(root.name, root.detail, !!root.children),
          sort: '0' + root.name,
        }))
      }
      return { suggestions: out }
    },
  }
  for (const lang of ['yaml', 'gotemplate', 'plaintext']) {
    disposables.push(monaco.languages.registerCompletionItemProvider(lang, templateProvider))
  }
  return { dispose() { for (const d of disposables) d.dispose() } }
}

// 通用类型规则兜底：bool → true/false；mode → 常用权限位。mode 值带
// 引号插入（裸 0755 会被 YAML 当八进制数解析，仓库示例统一 "0644" 写法）
const MODE_ENUMS = ['"0755"', '"0644"', '"0600"', '"0750"', '"0700"', '"0640"', '"0775"']

// 值补全：按键分发数据源（路径/相位/主机组等），枚举值按模块参数定位。
// 每个候选都带 doc：补全弹窗即文档——不熟悉的人在候选上就能看懂该填什么。
function valueSuggestions(
  deps: CompleteDeps, key: string, module: string | undefined, quoted: boolean,
  range: Monaco.IRange, K: typeof Monaco.languages.CompletionItemKind,
  model: Monaco.editor.ITextModel, position: Monaco.Position,
): Monaco.languages.CompletionItem[] {
  const fs = deps.fs
  const meta = deps.meta()
  if (!meta) return []
  const mods = deps.modules() || []
  const paths = fs.allFiles().filter((f) => !f.deleted).map((f) => f.path)
  const fileMap = new Map(fs.allFiles().filter((f) => !f.deleted && !f.binary).map((f) => [f.path, f.content]))
  const mk = (label: string, detail: string, doc?: string, sort = '0') => ({
    label, kind: K.File, insertText: label, range,
    detail,
    documentation: doc ? { value: doc } : undefined,
    sortText: sort + label,
  })
  // 已输开引号时插入裸值并顺手闭合引号；未输引号按原样（mode 带引号防八进制）
  const stripQ = (v: string) => (quoted ? v.replace(/^"|"$/g, '') + '"' : v)

  // 模块参数枚举（固定值域 + 类型规则）：state 这类值域在模块源码的
  // ParamDoc.Enum 声明、与解析白名单同源，经 /api/modules 透出——前端
  // 零硬编码，模块加值域即自动出现在补全里
  if (module) {
    const pdoc = mods.find((m) => m.name === module)?.params?.find((p) => p.name === key)
    if (pdoc?.enum?.length) {
      const head = `${module}.${key} 的固定值域。${pdoc.desc}`
      return pdoc.enum.map((v) => ({
        label: v, kind: K.EnumMember, insertText: stripQ(v), range,
        detail: `${module}.${key}`,
        documentation: { value: `${head}\n\n当前候选 **${v}**` },
        sortText: '0' + v,
      }))
    }
    if (pdoc?.type === 'bool') {
      return ['true', 'false'].map((v) => ({
        label: v, kind: K.EnumMember, insertText: v, range,
        detail: `${module}.${key} · bool`,
        documentation: { value: `布尔参数（${pdoc.desc}）：${v === 'true' ? '启用' : '关闭'}该行为` },
        sortText: '0' + v,
      }))
    }
    if (pdoc?.type === 'mode') {
      const explain: Record<string, string> = {
        '"0755"': 'rwxr-xr-x：目录/可执行常用（所有人可进、属主可改）',
        '"0644"': 'rw-r--r--：普通配置文件常用（属主可写，其余只读）',
        '"0600"': 'rw-------：仅属主可读写（密钥/凭据类文件）',
        '"0750"': 'rwxr-x---：属组可进，其他人无权限',
        '"0700"': 'rwx------：仅属主（私有目录/脚本）',
        '"0640"': 'rw-r-----：属组只读（服务进程读配置常用）',
        '"0775"': 'rwxrwxr-x：共享目录（组内可写）',
      }
      return MODE_ENUMS.map((v) => ({
        label: v.replace(/"/g, ''), kind: K.EnumMember, insertText: stripQ(v), range,
        detail: '常用权限位（引号写法防 YAML 八进制解析）',
        documentation: { value: `${explain[v]}\n\n必须带引号：裸 0755 会被 YAML 解析成八进制数` },
        sortText: '0' + v,
      }))
    }
  }

  switch (key) {
    case 'name': {
      // chart map 内的 name → 子 chart 名（裸 playbook 编辑器无 charts/
      // 上下文，paths 只含 playbook.yaml，天然无候选）
      return subchartNames(paths).map((n) => ({
        ...mk(n, '子 chart', `引用 charts/${n}/ 子 chart：本任务展开为该子 chart 的相位任务序列，此处填它的目录名`, '0'),
        kind: K.Module,
      }))
    }
    case 'src': {
      const cands = paths.filter((p) => !['chart.yaml', 'values.yaml', 'deploy.yaml'].includes(p))
      return cands.map((p) => mk(p, 'chart 内文件', `chart 根的相对路径。该文件已随包保存，模块下发时按此路径取（前端无需配置）`))
    }
    case 'include':
      return paths.filter((p) => /\.ya?ml$/.test(p) && !['chart.yaml', 'values.yaml'].includes(p))
        .map((p) => mk(p, '任务片段', '任务片段文件（顶层是任务列表）：`include: <路径>` 在加载期静态展开，与写在当前位置等价'))
    case 'values_from':
      return paths.filter((p) => /^(values|envs)\//.test(p))
        .map((p) => mk(p, 'values 覆盖文件', 'chart 内的 values 覆盖文件：按顺序深合并到当前作用域（本文件优先级高于 chart 默认 values）'))
    case 'chart':
      return subchartNames(paths).map((n) => ({
        ...mk(n, '子 chart', '`chart: <名>` 引用 charts/ 下的子 chart，展开为任务序列；附加参数写在同级（values/values_from/hosts/phase）', '0'),
        kind: K.Module,
      }))
    case 'tasks_from':
    case 'phase': {
      // 相位域 = 引用对象：优先取光标所在任务的 chart 值（taskChartRef 按
      // ctx.ts 的缩进语义定位任务块——多 chart 引用时各任务各归各的域）。
      // 定位失败（光标不在任务块内/本任务不是 chart 引用/引用名是模板
      // 变量）回退旧启发式：扫全部文件取第一个 chart: 键——多 chart 引用
      // 场景可能取错域，仅兜底
      let sub = taskChartRef(model.getValue(), position)
      if (!sub) {
        for (const content of fileMap.values()) {
          const m = content.match(/^\s*chart:\s*([A-Za-z0-9][\w.-]*)\s*$/m)
          if (m) { sub = m[1]; break }
        }
      }
      return phaseNames(paths, sub).map((p) => ({
        ...mk(p, sub ? `${sub} 相位` : '相位', '引用 chart 的相位（对应 <相位名>.yaml）：deploy=部署，uninstall=卸载；缺省 deploy', '0'),
        kind: K.EnumMember,
      }))
    }
    case 'hosts': {
      const hg = deps.hostGroups()
      if (!hg) return []
      return [
        { ...mk('all', '全部主机', '当前选择器命中的全部主机（最常用）', '0'), kind: K.EnumMember },
        ...hg.groups.map((g) => ({
          ...mk(g, '主机组', `台账里组 ${g} 的成员主机（与执行选择器取交集；组内无选中成员时该 play 跳过）`, '0'),
          kind: K.EnumMember,
        })),
        ...hg.hosts.map((h) => ({
          ...mk(h, '主机', '点名单台主机（精确目标；通常用组而非点名）', '1'),
          kind: K.Value,
        })),
      ]
    }
    case 'notify':
      return handlerNames(fileMap, controlKeys(meta)).map((n) => ({
        ...mk(n, 'handler', `处理器 ${n}：本任务成功（changed）时标记触发，play 末尾统一执行（可多任务合并触发一次）`, '0'),
        kind: K.Reference,
      }))
    default:
      return []
  }
}
