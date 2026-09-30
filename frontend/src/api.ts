// 后端 API 封装：JSON fetch + 统一错误口径 + 401 全局事件。
// 任何请求收到 401 都派发 wdp-unauthorized（App 切回登录视图）。

export interface Host {
  ID: number
  Name: string
  Address: string
  AgentPort: number
  Pools: string[] | null
  Groups: string[] | null
  Labels: string
  Status: string
  AgentBuild?: string // 最近在线上报的 agent 版本（升级门控：与 server build 一致=已最新）
  LastSeenAt: string
  CreatedAt: string
  UpdatedAt: string
  // 明文通道：agent 未启用 mTLS 时需显式打开（默认按 mTLS 建连并校验身份）
  AllowPlaintext?: boolean
}

export interface Pool {
  ID: number
  Name: string
  Note: string
  Members: number
  CreatedAt: string
}

export interface GroupEntry {
  ID: number
  Name: string
  Note: string
  CreatedAt: string
}

export interface LabelDef {
  ID: number
  Key: string
  Note: string
  CreatedAt: string
}

export interface BatchResult {
  Row?: number // 批量建档时 = 提交数组中的序号（1 起）
  ID: number
  Name: string
  OK: boolean
  Detail?: string
  Retired?: boolean
}

export interface BatchResponse {
  results: BatchResult[]
  ok: number
  failed: number
}

export interface EnrollTokenResp {
  token: string
  expires_in: number
  command: string
}

export interface ProbeResult {
  status: string
  scheme?: string
  error?: string
  hostname?: string
  version?: string
  build?: string
  goos?: string
  arch?: string
  cert_not_after?: string
  checked_at: string
}

// 主机详情（GET /api/hosts/{id}/facts）：探活快照 + setup 模块采集的 facts
export interface HostFactsResponse {
  probe: ProbeResult
  facts?: {
    hostname: string
    kernel: string
    arch: string
    default_ipv4: string
    euid: number
    cpus: number
    memory_mb: number
    os: { id: string; name: string; version: string; family: string }
    disk: { total_bytes: number; used_bytes: number; avail_bytes: number; use_percent: number }
  }
  error?: string
}

// handleResponse 响应统一处理：401 全局事件 + JSON 容错解析 + error 字段
// 提取。api() 与 upload() 共用一份——此前两处逐行复制，401 事件与错误
// 口径需双处同步维护。
//
// loginUrl 的 401 是「凭据错误」而非「会话失效」：登录端点必须豁免全局
// 401 处理，否则输错密码会提示「登录已失效」并触发全局跳登录（人就在
// 登录页），真实原因（服务器返回 invalid credentials）反而被吞。
const loginUrl = '/api/login'

async function handleResponse<T>(resp: Response, url?: string): Promise<T> {
  if (resp.status === 401 && url !== loginUrl) {
    window.dispatchEvent(new Event('wdp-unauthorized'))
    throw new Error('登录已失效')
  }
  const data = (await resp.json().catch(() => ({}))) as T & { error?: string }
  if (!resp.ok) {
    throw new Error(data?.error || `HTTP ${resp.status}`)
  }
  return data
}

export async function api<T>(method: string, url: string, body?: unknown): Promise<T> {
  // Content-Type 恒带（含空体 POST）：后端对变更类请求拒绝空 Content-Type
  // （CSRF 纵深防御），无体调用（probe/latest 等）不带会被 415 挡下
  const opt: RequestInit = {
    method,
    headers: { Accept: 'application/json', 'Content-Type': 'application/json' } as Record<string, string>,
  }
  if (body !== undefined) {
    opt.body = JSON.stringify(body)
  }
  const resp = await fetch(url, opt)
  return handleResponse<T>(resp, url)
}

export interface App {
  ID: number
  Name: string
  Note: string
  LatestVersion: string
  Pools: string[] | null
  Groups: string[] | null
  Labels: string
  VersionCount: number
  CreatedAt: string
  UpdatedAt: string
}

export interface AppVersion {
  ID: number
  AppID: number
  Version: string
  Sha256: string
  Size: number
  Note: string
  Phases: string[] | null
  CreatedAt: string
}

// 运行时设置（admin 设置页）：全库单文档，nil 字段 = 未配置（回落
// 启动参数/默认），显式 0 合法（如保留 0 = 永久）
export interface SettingsDoc {
  probe_every_sec?: number
  session_ttl_min?: number
  alert_warn_pct?: number
  alert_crit_pct?: number
  metrics_retain_days?: number
  runs_retention_days?: number
  audit_retention_days?: number
  drafts_retention_days?: number
  allow_plaintext_enroll?: boolean
}

export interface SettingsView extends SettingsDoc {
  version: number
  updated_at?: string
  updated_by?: string
}

export interface Run {
  ID: number
  Kind: string
  AppID: number
  AppName: string
  Version: string
  Phase: string
  Seq: number
  Status: string
  Selector: string
  Summary: string
  User: string
  StartedAt: string
  FinishedAt: string
}

// 用户与会话管理（user:manage）
export interface UserScope {
  verb: string
  kind: string // '' = 全部 | pool | group | label
  value: string
}

export interface User {
  id: number
  name: string
  role: string
  disabled: boolean
  created_at: string
  scopes: UserScope[] | null
  online: boolean
}

export interface SessionInfo {
  token: string
  user: string
  expires: string
}

// 操作审计（GET /api/audit）
export interface AuditLog {
  ID: number
  User: string
  Action: string
  Object: string
  Name: string
  Detail: string
  IP: string
  CreatedAt: string
}

// 主机健康告警（GET /api/alerts；页面红黄标记，真正告警走 Prometheus）
export interface HostAlert {
  HostID: number
  HostName: string
  Kind: string // cpu / mem / fs / offline
  Level: string // warn / crit
  Detail: string
  Value: number
  UpdatedAt: string
}

// Prometheus 文本格式的解析样本（GET /api/hosts/{id}/metrics?format=json）
export interface MetricSample {
  name: string
  labels?: Record<string, string>
  value: number
}

// 5 分钟聚合桶（GET /api/hosts/{id}/series）
export interface SeriesPoint {
  ts: number
  avg: number
  max: number
}

// 该主机最近的执行任务（GET /api/hosts/{id}/tasks）
export interface HostTaskItem {
  RunID: number
  AppName: string
  Kind: string
  Task: string
  Module: string
  Status: string
  Changed: boolean
  Detail: string
  StartAt: string
}

export interface RunTask {
  ID: number
  RunID: number
  Play: string
  Task: string
  Module: string
  Host: string
  Status: string
  Changed: boolean
  Detail: string
}

// 字段名必须与后端 json tag 一致（小写）
export interface ExecHostResult {
  id: number
  name: string
  code: number
  stdout: string
  stderr: string
  err?: string
}

// multipart 上传（应用 tgz）；调用方显式声明响应类型。
export async function upload<T = unknown>(url: string, fields: Record<string, string>, file: File): Promise<T> {
  const fd = new FormData()
  for (const [k, v] of Object.entries(fields)) fd.append(k, v)
  fd.append('tgz', file)
  const resp = await fetch(url, { method: 'POST', body: fd, headers: { Accept: 'application/json' } })
  return handleResponse<T>(resp, url)
}

export interface SpecFile {
  path: string
  content?: string
  binary?: boolean
  size: number
}

// 模块自描述（GET /api/modules）：编辑器动态表单的数据源，新增模块自动出现
export interface ModuleParamDoc {
  name: string // "(free-form)" = 简写参数（如 shell: uptime）
  type: string // string / list / bool / int / mode / map
  default?: string
  desc: string
  enum?: string[] | null // 固定值域（后端解析白名单同源；值补全/文档消费）
}

export interface ModuleMeta {
  name: string
  desc: string
  free_form: boolean
  params: ModuleParamDoc[] | null
  example?: string
  rollback: number // 0=无 1=部分 2=全量
  read_only: boolean
}

export interface AppSpec {
  name: string
  version: string
  description: string
  values_yaml: string
  deploy_yaml: string
  files: SpecFile[] | null
  pools: string[] | null
  groups: string[] | null
  labels: string
}

// ---- IDE 支撑（schema / validate / draft）----

// GET /api/schema：结构元数据（与后端 model.FieldSection 对应，无 json
// tag 按导出名序列化）
export interface FieldDoc {
  Name: string
  Type: string
  Default?: string
  Desc: string
}

export interface FieldSection {
  Title: string
  Fields: FieldDoc[]
  Example?: string
}

export interface SchemaMeta {
  task: FieldSection[]
  play: FieldSection[]
  chart: FieldSection[]
  builtin_vars: string[]
  template_funcs: string[]
}

// POST /api/apps/validate 的发现（line 为后端结构化行号，0/缺省 = 未知）
export interface ValidateIssue {
  level: string // ERROR / WARN
  path: string // chart 内相对路径（'' = 整体性问题）
  msg: string
  line?: number
}

// 保存/校验请求体（specReq 的 IDE 形态；三件套以 verbatim 文件提交）
export interface SpecSaveBody {
  name?: string // 新建模式必填
  version: string
  description: string
  files: { path: string; content: string; size?: number }[]
  delete_files: string[]
  base_version: string
  pools: string[]
  groups: string[]
  labels: string
}

// GET /api/apps/draft
export interface DraftResp {
  payload: string
  base_version: string
  updated_at: string
}

// ---- 具名 API（资源级封装层）----
// 视图不散落字符串 URL 与请求形状：路径、参数、响应类型集中在这里，
// 接口增删改只动这一处（调用方只见函数签名与类型）。新功能一律走这
// 层，存量内联调用在触及时逐步迁移。

// GET /api/apps/drafts：草稿箱列表（当前用户）
export interface AppDraftItem {
  key: string // "<appID>"（编辑中）| "new:<name>"（新建中）
  kind: 'new' | 'edit'
  app_name: string
  app_gone: boolean // 底本应用已删除（草稿仅可清理，不可继续编辑）
  base_version: string
  updated_at: string
}

export function listAppDrafts(): Promise<AppDraftItem[]> {
  return api('GET', '/api/apps/drafts')
}

export function deleteAppDraft(key: string): Promise<void> {
  return api('DELETE', `/api/apps/draft?key=${encodeURIComponent(key)}`)
}

// POST /api/playbook/validate：裸 playbook 内容级校验（单文件，非 chart）
export async function validatePlaybook(content: string): Promise<ValidateIssue[]> {
  const resp = await api<{ issues: ValidateIssue[] | null }>('POST', '/api/playbook/validate', { content })
  return resp.issues || []
}

// POST /api/apps/spec：图形化新建应用（chart.yaml 须在 files 里，版本与
// 之对账；deploy.yaml 支持 play 形态——playbook 内容可直接作为部署相位）
export function createAppFromSpec(body: SpecSaveBody): Promise<App> {
  return api('POST', '/api/apps/spec', body)
}

// GET /api/apps/{id}/download：下载版本制品（tgz；cookie 会话直接导航即可，
// Content-Disposition: attachment 触发浏览器下载而不离开当前页）
export function appDownloadURL(appID: number, version?: string): string {
  return `/api/apps/${appID}/download${version ? `?version=${encodeURIComponent(version)}` : ''}`
}

// ---- run 事件流（SSE）----
// 执行状态变化的服务端推送：取代高频全量轮询（读放大主源）。返回断开
// 函数；EventSource 同源自动带会话 cookie。连接层断线由 EventSource
// 自动重连；HTTP 错误（如 401/502）后不再重连，错过的状态靠调用方的
// 兜底轮询补齐。
export interface RunEvent {
  id: number
  status: string
  summary?: string
}

export function subscribeRuns(onEvent: (e: RunEvent) => void, onError?: () => void): () => void {
  const es = new EventSource('/api/runs/stream')
  es.addEventListener('run', (ev) => {
    try {
      onEvent(JSON.parse((ev as MessageEvent).data) as RunEvent)
    } catch { /* 坏载荷忽略：兜底轮询会补 */ }
  })
  // HTTP 错误（server 重启期间的 502、反代超时）会让 EventSource fail 且
  // 不再自动重连——不通知调用方的话，它把"收到过事件"当永久状态，兜底
  // 轮询从此卡在慢档
  if (onError) es.onerror = () => onError()
  return () => es.close()
}

// ---- exec 结果实时流（SSE）----
// POST /api/exec 立即返回 run_id，逐主机完成即推送 host 事件（完整
// stdout/stderr），run 收尾推送 done 后服务端关流。返回断开函数。
// 连接断开时 EventSource 自动重连，服务端会完整重放缓冲（按 id 幂等
// 覆盖即可）；HTTP 错误不重连，调用方 onError 落库兜底。
export interface ExecStreamHost {
  result: ExecHostResult
  status: string
}

export interface ExecStreamDone {
  ok: number
  failed: number
}

export function subscribeExecStream(
  runId: number,
  onHost: (h: ExecStreamHost) => void,
  onDone: (d: ExecStreamDone) => void,
  onError?: () => void,
): () => void {
  const es = new EventSource(`/api/exec/stream?run_id=${runId}`)
  es.addEventListener('host', (ev) => {
    try {
      onHost(JSON.parse((ev as MessageEvent).data) as ExecStreamHost)
    } catch { /* 坏载荷忽略：重连重放会补 */ }
  })
  es.addEventListener('done', (ev) => {
    es.close()
    try {
      onDone(JSON.parse((ev as MessageEvent).data) as ExecStreamDone)
    } catch {
      onDone({ ok: 0, failed: 0 })
    }
  })
  if (onError) es.onerror = () => onError()
  return () => es.close()
}
