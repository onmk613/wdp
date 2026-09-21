// Monaco 接入：动态加载（只在 IDE 路由引入，不拖累控制台首屏）、worker
// 本地打包（内网部署，禁止 CDN）、yaml 诊断由 monaco-yaml 提供、
// gotemplate 自定义语言（{{ }} 模板文件的着色）。
//
// 所有 provider 的注册在 setupMonaco 一次性完成（幂等），语言特性拿到
// 的是共享的 IDE 上下文对象（fs/schema/modules），内容变化时上下文自身
// 更新即可，无需反复注册 provider。

import type * as Monaco from 'monaco-editor'

export type MonacoNs = typeof Monaco

let monacoPromise: Promise<MonacoNs> | null = null

export function modelURI(path: string): Monaco.Uri {
  // 自定义 scheme：fileMatch 按完整 uri 匹配，路径即 chart 相对路径
  return (monacoRef() as any).Uri.parse(`wdpchart:/${path}`)
}

function monacoRef(): MonacoNs | null {
  return (window as any).__wdpMonaco ?? null
}

// IDE 上下文：provider 闭包捕获这份可变对象（内容变化时 AppIDE 更新它）
export interface IDECtx {
  getText: (path: string) => string | undefined // 当前文件内容（fs/草稿为准）
  listPaths: () => string[] // 全部未删除文件
}

export async function loadMonaco(): Promise<MonacoNs> {
  if (monacoPromise) return monacoPromise
  monacoPromise = (async () => {
    const [monaco, yamlMod, editorWorker, yamlWorker] = await Promise.all([
      import('monaco-editor'),
      import('monaco-yaml'),
      import('monaco-editor/esm/vs/editor/editor.worker?worker'),
      import('monaco-yaml/yaml.worker?worker'),
    ])
    ;(window as any).__wdpMonaco = monaco
    ;(self as any).MonacoEnvironment = {
      getWorker(_: string, label: string) {
        if (label === 'yaml') return new (yamlWorker as any).default()
        return new (editorWorker as any).default()
      },
    }
    // monaco-yaml 注册 yaml 语言与诊断；schema 由 schemas.ts 按文件装配后
    // 重新调用 configureMonacoYaml 更新（可重复配置，返回 IDisposable 不持有）
    // completion 关闭：键补全由 complete.ts 的 provider 独占（模块名/
    // 参数/控制键带中文文档与骨架）。schema 补全开着会出第二套同名
    // 候选（when/become/name/chart…两份，且无文档），用户看到重复项。
    // validate（语法+schema 类型校验）与 hover（字段文档）保留
    ;(yamlMod as any).configureMonacoYaml(monaco, {
      enableSchemaRequests: false,
      validate: true,
      completion: false,
      hover: true,
      format: false,
      schemas: [],
    })
    registerGoTemplate(monaco)
    applyTheme(monaco)
    return monaco
  })()
  return monacoPromise
}

// configureYaml 重新装配 monaco-yaml 的 schema 集合（文件增删/改名后
// 调用；fire-and-forget，调用方不等待）
export function configureYaml(opts: any): void {
  void loadMonaco().then((monaco) => {
    void import('monaco-yaml').then((m: any) => m.configureMonacoYaml(monaco, opts))
  })
}

// gotemplate：{{ }} 模板语言（*.tpl 与含 {{ 的配置模板）。Monarch 着色
// 足够（关键字/字符串/变量引用），补全由 complete.ts 的通用 provider 覆盖。
function registerGoTemplate(monaco: MonacoNs) {
  if (monaco.languages.getLanguages().some((l) => l.id === 'gotemplate')) return
  monaco.languages.register({ id: 'gotemplate', extensions: ['.tpl'] })
  monaco.languages.setMonarchTokensProvider('gotemplate', {
    defaultToken: '',
    tokenPostfix: '.tpl',
    brackets: [{ open: '{{', close: '}}', token: 'delimiter.curly' }],
    tokenizer: {
      root: [
        [/\{\{\s*\/\*[\s\S]*?\*\/\s*\}\}/, 'comment'],
        [/\{\{-?/, { token: 'delimiter.bracket', next: '@tmpl' }],
        [/-?\}\}/, 'delimiter.bracket'],
        [/"([^"\\]|\\.)*"/, 'string'],
        [/'([^'\\]|\\.)*'/, 'string'],
      ],
      tmpl: [
        [/-?\}\}/, { token: 'delimiter.bracket', next: '@pop' }],
        [/\b(if|else|else if|end|range|with|define|template|block|include|nil|and|or|not|len|index|printf)\b/, 'keyword'],
        [/\.[A-Za-z_][\w]*/, 'variable'],
        [/"([^"\\]|\\.)*"/, 'string'],
        [/'([^'\\]|\\.)*'/, 'string'],
        [/[=<>!]+/, 'operator'],
        [/\s+/, ''],
        [/-?\}\}/, { token: 'delimiter.bracket', next: '@pop' }],
        [/[^{}"']+/ , ''],
        [/\{\{/, 'invalid'],
      ],
    },
  } as any)
}

// 主题：浅色基础 + 模板变量/控制键用 decorations 着色（CSS 类在
// AppIDE.vue 里定义），编辑器本身保持默认主题的对比度
function applyTheme(monaco: MonacoNs) {
  monaco.editor.defineTheme('wdp', {
    base: 'vs',
    inherit: true,
    rules: [
      { token: 'delimiter.bracket', foreground: '7c3aed' },
      { token: 'keyword', foreground: '2563eb' },
      { token: 'variable', foreground: '0891b2' },
    ],
    colors: {
      // 注意：Monaco 主题色必须是合法 #RRGGBB(AA) 十六进制；无 # 的 6 位
      // 值解析失败会把 CSS 变量污染成纯红（gutter 背景曾整条变红）
      'editorLineNumber.foreground': '#a8b3c4ff',
      'editorGutter.background': '#fafbfdff',
    },
  })
}

// bindSuggestKey 手动补全触发键：macOS 的 Ctrl+Space 被系统输入法切换
// 占用，补一个 Alt(Option)+Space 绑定（跨平台生效，编辑器创建后调用）
export function bindSuggestKey(monaco: MonacoNs, editor: Monaco.editor.IStandaloneCodeEditor): void {
  editor.addCommand(monaco.KeyMod.Alt | monaco.KeyCode.Space, () =>
    editor.trigger('keyboard', 'editor.action.triggerSuggest', null))
}

export function editorOptions(monaco: MonacoNs): Monaco.editor.IStandaloneEditorConstructionOptions {
  return {
    theme: 'wdp',
    automaticLayout: true,
    tabSize: 2,
    insertSpaces: true,
    detectIndentation: false,
    minimap: { enabled: false },
    scrollBeyondLastLine: false,
    fontSize: 13,
    renderWhitespace: 'boundary',
    quickSuggestions: { other: true, comments: false, strings: true },
    suggestOnTriggerCharacters: true,
    suggest: {
      // 模块补全插入的是多行 snippet 骨架（参数占位符逐个 Tab 填值）。
      // Monaco 默认在 snippet 会话存续期间抑制 quickSuggestions——用户
      // 正是在占位符里逐字段配参数，值补全/枚举会全被关掉，看起来
      // "模块整体补全后子字段没有补全"。这里显式关闭该抑制
      snippetsPreventQuickSuggestions: false,
    },
    // 词补全关闭：文档词（when/shell/name…）会和 provider 的同名候选
    // 形成第二套重复项；模块/参数/变量补全已由 provider 全覆盖
    wordBasedSuggestions: 'off',
    parameterHints: { enabled: false },
    // 中文 chart 里全角标点（（）、——）会被默认 unicodeHighlight 框成
    // 黄色小框，对中文用户是噪音，关掉
    unicodeHighlight: { ambiguousCharacters: false, invisibleCharacters: false, nonBasicASCII: false },
    lineNumbersMinChars: 3,
  }
}

// languageFor 按路径选语言
export function languageFor(path: string): string {
  if (/\.ya?ml$/.test(path)) return 'yaml'
  if (/\.tpl$/.test(path)) return 'gotemplate'
  if (/\.json$/.test(path)) return 'json'
  if (/\.s?html?$/.test(path)) return 'html'
  if (/\.sh$/.test(path)) return 'shell'
  if (/\.(service)$/.test(path)) return 'ini'
  if (/\.conf$|\.cfg$|\.ini$|\.env$/.test(path)) return 'ini'
  if (/\.py$/.test(path)) return 'python'
  if (/\.sql$/.test(path)) return 'sql'
  if (/\.xml$/.test(path)) return 'xml'
  if (/\.md$/.test(path)) return 'markdown'
  return 'plaintext'
}
