# 17 · Web 控制台会话工作记录（2026-09-18）

> 本轮会话在 `web-console` 分支上完成的全部改造。所有改动均通过后端测试
> （28 个包）与浏览器端到端验证；前端产物已同步至 `internal/web/static/`。
> 按主题分节，含设计取舍与验证结论，供后续窗口/协作者接续。

---

## 1. 前端路由化（history 模式）

**动机**：SPA 无 URL 路由——刷新丢状态、无法深链分享、前进后退失效。
API 本就有固定 path（`/api/*`），前端路由与 API 无关。

- 引入 `vue-router@4`（`frontend/src/router.ts`）：`/hosts`、`/hosts/:id`、
  `/exec`、`/runs`、`/audit`、`/apps`、`/apps/new`、`/apps/:id/edit?base=`、
  `/apprun`、`/pools|groups|labels`；未知路径兜底回 `/hosts`。
- 后端 SPA fallback（`internal/web/static.go`）：非 `/api/*`、`/enroll/*`
  的未匹配 GET 回退 index.html；保留前缀下未知路径维持 404（API 客户端
  不拿到 HTML）。
- 登录页可被深链穿透（登录后回到原路由）；浏览器前进后退正常。

## 2. 主机批量录入与 SSH 批量安装

- **CSV 固定列序**：手动添加 `name,address,agent_port,pools,groups,labels`；
  SSH 安装 `name,address,ssh_port,user,password`（行内 user/password 优先，
  空则用对话框共享凭据）。单元格多值用分号。
- 单列 IP 行合法（name = IP，其余默认）；`#` 注释行、模版头行自动跳过；
  同批次重名/非法端口/缺 address 逐行标错，不阻断有效行。
- 前端 `frontend/src/csv.ts`（解析+模版+BOM Excel 兼容）；手动添加走新端点
  `POST /api/hosts/import`（逐台结果、上限 1000）；SSH 批量前端并发 3 复用
  单台 `/api/agents/install`，逐台状态表（排队/安装中/已上线/失败）+ 仅重试
  失败行。
- 模版下载：`wdp-hosts.csv` / `wdp-ssh-hosts.csv`（带注释示例）。

## 3. 分类管理（池/组/标签）

- 新菜单「分类管理」三个子页（`/pools` `/groups` `/labels`，共用
  `ScopePage.vue`）：注册表 CRUD + 成员视图。
- 成员点击跳转：主机 chip → `/hosts?search=<名>` 定位；应用 chip →
  `/apps/:id/edit` 编辑器。台账引用但未注册的分类名标「未注册」。

## 4. 应用版本级作用域（scope 继承修复）

**问题**：编辑器按底本版本加载时，池/组/标签回填的是应用级（最近保存），
不是底本版本自己的——基于旧版本编辑会拿错 scope。

- `app_versions` 加 `pools/groups/labels` 列（schema 迁移自动升级）；`AddVersion` 随版本
  落 scope；`GET /api/apps/{id}/spec?version=X` 返回 X 版本自己的 scope；
  应用编辑对话框保存同步默认版本行；chart 上传继承应用级；旧行回退应用级。
- 会话后段复用同机制补了 `chart_meta` 全字段往返（见 §7）。

## 5. 执行记录关联用户 + 操作审计

- `runs` 加 `user` 列（exec/应用执行记录触发者）；执行记录页「操作者」列。
- 新表 `audit_logs`（user/action/object/name/detail/ip/created_at）+
  `GET /api/audit?q=&limit=`。埋点覆盖：主机增删改/导入/批量/SSH 推装、
  池组标签、应用与版本全生命周期、run 删除、登录成功/失败/登出、纳管 token。
  执行类不进审计（runs 已带 user，两者互补）。
- 「操作日志」菜单页（`/audit`）：动作彩色标签（增绿/改橙/删红）、搜索、
  长内容悬浮看全（名称 show-overflow-tooltip、详情 tooltip 气泡）。

## 6. 新建应用独立页 + 编辑器全字段适配

- 「新建应用」独立菜单（`/apps/new` → `AppNew.vue`）：图形化编辑器与
  chart tgz 上传双入口；应用列表原按钮移除。
- **模块参数动态表单**：新端点 `GET /api/modules` 暴露全部 23 个内置模块
  的自描述（params 表/示例/free-form/回滚能力/只读）。任务抽屉
  （`components/TaskDrawer.vue`）按此渲染表单——**新增模块（实现
  UsageProvider）自动获得表单，零前端改动**；未知模块（chart 本地脚本
  模块）退化为 YAML 参数框。
- 任务控制属性全字段（when/loop/until/retries/environment/become 三态/
  delegate_to/run_once/register/notify/tags/changed_when/failed_when/
  output/no_log/hook…）+ chart 引用与 include 专属面板；不认识的键进
  extra 原样带回（`frontend/src/appedit.ts` 双向转换）。
- 多 play 支持（play 卡片 + 高级属性 YAML）；chart.yaml 元数据全字段表单
  （`chart_meta`：required/marker_dir/no_marker/check_mode 三态/
  inventory_override/sensitive_values/phases 声明表），后端 spec 读写全量
  往返，API 调用不带 chart_meta 时保留底本字段。

## 7. 主机监控四层方案

架构（讨论定案）：**agent 内置精简采集器（命名对齐 node_exporter）+
server 5 分钟聚合存储 30 天 + 页面轻告警标记 + 代理端点供 Prometheus**。
不部署 node_exporter 进程、不自建 TSDB、不做通知渠道（真告警交给
Prometheus+Grafana）。

1. **agent `/metrics`**（`internal/agent/metrics*.go`）：`node_cpu_seconds_total`
   （按核×模式）、memory、filesystem（真实挂载点）、diskstats、网卡流量、
   boot time；挂现有 mTLS 端口。Linux /proc + statfs；darwin sysctl 兜底
   子集。**平台差异按 build tag 拆文件**（metrics_darwin.go /
   metrics_fs_unix.go / metrics_fs_other.go）——曾因直接引用 darwin 专属
   符号导致交叉编译失败，已修复并四平台回归。
2. **采集/聚合**：scrapeLoop 每分钟抓取 → 计数器差分派生（CPU 使用率、
   网络速率、磁盘繁忙）→ 并入 5 分钟桶（n/vsum/vmax，avg=vsum/n）→
   `metrics_5m` 表，每小时清理 30 天前数据。
3. **页面轻告警**：阈值评估写 `host_alerts`（cpu/mem ≥85 黄 ≥95 红、
   fs ≥88 黄 ≥95 红、offline 红）；主机列表状态列红/黄感叹号点击弹详情；
   恢复自动清除（实测停 agent 65s 出红告警、重启后下轮清除）。
4. **Prometheus 网关**：`GET /api/hosts/{id}/metrics` 原文代理（mTLS 探活
   定 scheme）；`requireAuth` 对 GET 支持 **HTTP Basic**（bcrypt 校验，
  401 带 WWW-Authenticate）——Prometheus 配 basic_auth 即可抓取。
   `?format=json` 供控制台实时页。
5. **主机详情独立页** `/hosts/:id`：基本信息、实时快照（30s 刷新）、
   趋势图、setup facts、该主机执行历史。主机名点击进入（抽屉已移除）。
6. **趋势图**（`components/Sparkline.vue`，纯 SVG 无图表库）：纵横轴
   nice 刻度、悬停十字线+数值气泡（时间/均值/峰值）、峰值虚线；
   快捷 6h/24h/7d/30d + datetimerange 自定义区间（series 端点支持
   from/to 任意窗口）。

## 8. agent 远程升级

**动机**：server 迭代后旧 agent 无新能力，只能删主机重建。

- 流程完全在 server 侧编排，只依赖旧 agent 已有的 `PUT /file` + `POST
  /exec`——最老的 agent 也能升级：探活（平台/版本/bin_path）→ 同版本且
  未 force 幂等短路 → server 同级 bin 目录取目标平台二进制（与 enroll
  同源）→ 上传到**替换目标同目录**临时文件 → `mv` 原子替换 → systemd
  restart → 轮询探活 90s 等新版本上线。
- 版本基建：`internal/buildinfo` 叶子包（解 `cli→agent→cli` import 环），
  build.sh ldflags 注入；agent `/health` 新增 `build`（`版本-commit`）与
  `bin_path`（升级替换的精准目标，旧 agent 由 `readlink /proc/$PPID/exe`
  兜底探测）。
- 端点：`POST /api/hosts/{id}/upgrade`（force 可选）、`POST /api/hosts/
  upgrade`（批量并发 3）；UI 操作列「升级」+ 批量栏；全程审计。
- 非 systemd 部署：替换二进制后明确提示手动重启，不假装成功。
- 真实闭环验证：伪造 `9.9.9` 旧版 → 升级 → `/health` 报新版本 → 幂等
  重升级「已是最新」。

## 9. 应用执行相位行级化

**动机**：相位原是执行目标区的全局单选（硬编码 deploy/status/uninstall），
一套组合只能全员同相位。行级化后：**相位是执行清单条目的属性**——同一
应用可多次入清单、各条目独立选相位（甚至同相位不同阶段），组合出
「先 backup 全部 → 逐个 deploy → 最后 status 巡检」这类编排。

- **相位数据源**：版本创建时（chart 上传 / 新建应用 / 编辑器保存三条
  路径）从制品提取 `chart.PhaseNames()`（deploy 在前，其余字典序）落
  `app_versions.phases` 列（版本不可变，提取一次即真）；`GET /api/apps/{id}`
  版本明细带 `Phases`。迁移前旧行为空 → 前端回退内置三相位。
- **API**：`RunItem` 加行级 `phase`（空 → 请求级 `phase` 兜底 → deploy，
  旧客户端不破坏）；预校验阶段即比对库中相位清单，未知相位 400 带可用
  列表（不再执行到一半才炸）。
- **run 记录带相位**：`runs` 加 `phase` 列；执行记录页与内联输出标题
  都显示相位标签——同应用多条 run 靠相位可区分。
- **前端**（`AppRunPage.vue`）：全局相位下拉移除；清单加「相位」列
  （行内 select，选项=该行所选版本的声明相位；列头 tooltip 说明语义）；
  版本切换后相位若不在新版本清单内自动重置 deploy；确认弹窗逐条目摘要
  `app@版本[相位] → …`。
- 验证：后端 e2e（同应用两行 deploy/status 各自跑对相位任务、未知相位
  400）+ 浏览器实测（本地 agent：run#1 deploy 输出 deploy-phase-done、
  run#2 status 输出 status-phase-done）。

## 10. 主机标签选择化 / 版本作用域就地变更 / play hosts 默认应用名

1. **标签行式编辑器**（`components/LabelRows.vue`）：主机编辑与批量设置
   对话框的标签从自由文本（`k=v, k2=v2`）改为行式「键下拉 + 值输入」——
   键下拉列出注册表已有标签键（filterable allow-create，可直接输入新键，
   已占用的键不再出现在其它行的选项里），值可空（纯键标签）。三处复用
   （主机编辑、批量设置、版本作用域）。实现要点：外部值 watch 与行
   watch 必须按内容比对（emit 每次新对象引用，直接 rebuild 会死循环，
   下拉被持续重渲染不可点）；空行（rows.length=0）时必须重建（初始 {}
   与空行内容相等，不能跳过初始化）。
2. **版本作用域就地变更**（不升版本）：新端点 `PUT /api/apps/{id}/scope`
   （`UpdateVersionScopes`）——指定 version（空=latest）直接改
   app_versions 行的池/组/标签；目标为 latest 时同步应用级（与编辑器
   保存同口径），改旧版本只动该版本行。应用列表版本行新增「作用域」
   按钮，对话框按该版本 spec 回填。后期的权限/归属调整不再被迫升版本。
3. **play hosts 默认 = 应用名**（`appedit.ts` / `AppEdit.vue`）：新建
   应用初始 play 与「添加 play」的 hosts 缺省填应用名（应用名变化时仍
   处于默认值 all/空的 play 跟随，用户改过的不动）；hosts 输入框带
   tooltip 说明。动机：inventory 设计以应用名分组，hosts: all 的 chart
   导出后 CLI 执行会打到全部主机——与 inventory 语义背道而驰。

## 11. 登录页路由化

- 登录从「App 条件渲染组件」改为真实路由 `/user/login`（独立全屏，无
  控制台布局）；控制台页面挂 Console 布局路由（嵌套子路由），根路径
  `/` → `/hosts`，未登录访问任意页由全局前置守卫重定向到
  `/user/login?redirect=<原地址>`（登录后深链回跳）；已登录访问登录页
  回控制台。
- 新增 `frontend/src/auth.ts`：认证态单例（authUser + /api/me Promise
  缓存），守卫/Console/登录页共享；App.vue 只做 401 事件收口。
- **两个踩坑**（都修在代码里，注释有记录）：① /api/me 的 401 不能走
  api()（会派发 wdp-unauthorized，初始导航挂起时事件处理器 push 登录页
  与守卫重定向互相打断 → 无限重定向循环）——auth 探测用裸 fetch；且
  401 push 前等 router.isReady()。② setAuth 必须同时更新 mePromise
  缓存，否则登录后首次跳转被守卫按旧值（未登录）弹回登录页。

## 12. 执行目标资源隔离 + web 压力优化

1. **执行目标按权限裁剪**：新端点 `GET /api/exec/targets`（run:execute
   范围内的主机 + restricted 标记）。远程执行页与应用执行页的目标数据
   源全部改用它——作用域用户的"全部主机/池/组/标签"选项从目标主机派生
   （不再取全局注册表），资源显示与执行权限同口径，不会再"选得到、
   跑不了"；restricted=true 时页面顶部提示。修连带 bug：/api/me 的
   role 在响应顶层而不在 perms 摘要里，ensureAuthed 直接赋值会丢 role
   （头部角色徽标恒显示 "-"）——合并顶层 role。
2. **web 压力优化**（不改架构的无悔项）：
   - http.Server 补 `ReadHeaderTimeout=15s`（防 slowloris 慢速连接）与
     `IdleTimeout=120s`（回收空闲连接）；**WriteTimeout 必须留 0**——
     远程命令/升级是同步长请求，写超时会掐断它们。
   - SQLite 加 `_pragma=synchronous(NORMAL)`：WAL 推荐档，高频小写
     （指标/审计）不再每笔 fsync。
   - 监控写入批量化：每主机一轮采样十几条样本收进单事务
     （`BatchUpsertMetric5m` prepared stmt），替代逐条自提交。
   - scrapeOnce 串行改 8 并发有界抓取：一台 agent 超时不再拖累整轮
     （主机规模大时 1 分钟周期装不下的问题）。
   - 容量结论：单实例多用户共享场景（数十用户 × 数百主机）无需拆分；
     瓶颈是 SQLite 单写者——写路径都是低频小事务，读并发 WAL 下无锁，
     够用。真正的扩展路径（只读副本/写分离/换 Postgres）等主机过万再议。

## 13. 其它修复

- 主机 facts 预览（后并入详情页）：`GET /api/hosts/{id}/facts` 复用 setup
  模块本体采集（单一事实来源），在线/离线双路径 e2e。
- 模块下拉切换不生效（受控组件漏写回值）、Prometheus 文本解析器标签
  切分 panic、审计页长字段截断（tooltip）、curl `-d` 默认 POST 踩坑等。

## 14. Chart 编辑器 IDE 重构（图形化双模式 → Monaco 纯文本 IDE）

决策：编辑器内核 Monaco（内网本地打包、IDE 路由动态加载）；lint ERROR
阻断保存（WARN 确认放行）；旧图形化编辑器（AppEdit/TaskDrawer/appedit.ts
约 1500 行双向转换层）整体删除。

后端（Go）：
- `GET /api/schema`：任务/play/chart 字段文档表（`playbook.TaskFieldSections`
  新增 + `ChartFieldSections`）+ 模板补全候选（`render.AllowlistFuncs`、
  `executor.BuiltinVarNames` 导出投影）——与 `wdp schema` CLI 同源。
- `POST /api/apps/validate`：编辑内容物化到临时目录 → chart.Load 自检 →
  `chart.Lint` 全量（模块名/chart 引用/模板可渲染/schema/envs/phases），
  200+issues 返回；`prepareSpecWorkspace` 从保存 handler 抽出共用。
- spec 保存/读取：三件套可 verbatim 提交（files 里 chart.yaml 原文落盘，
  注释保真；name/version 与请求对账，不一致 400）；`GET spec` 返回
  chart.yaml 原文进 files。
- 草稿：表 `app_drafts`（按用户隔离，key = `<appID>`/`new:<名>`，8MiB
  上限）；`GET/PUT/DELETE /api/apps/draft`。

前端（`frontend/src/ide/`）：
- ChartFS 内存文件系统是唯一状态源：保存/校验/草稿共用同一序列化；
  三件套是普通文件，无结构化重写层，注释逐字保真。
- Monaco + monaco-yaml（configureMonacoYaml 按 fileMatch 挂动态 schema：
  根相位 yaml/tasks 片段=playbook schema，chart.yaml=元数据 schema，
  values.yaml 可挂 chart 自带 values.schema.json）；gotemplate 自定义
  语言（*.tpl 着色）。
- 智能补全：缩进块上下文识别（ctx.ts）驱动模块名（含骨架 snippet）/
  模块参数/任务控制键/play 键；`{{ }}` 变量域（values 实时解析 + helpers
  define 名 + register/loop_var + 内置变量 + sprig 白名单函数，尾点下钻
  子级）；值补全（src/include 文件路径、chart 子包、tasks_from 相位、
  hosts 主机/组、notify handler 名）。
- 语义着色（yaml 包 AST + decorations）：模块键/控制键/play 键三色，
  when/loop/register 任务行号槽徽标。
- 暂存：服务端草稿 + localStorage 双写，自动暂存 2.5s debounce，进入时
  横幅恢复/丢弃；保存成功清草稿。
- 保存门禁：validate ERROR 阻断（问题面板定位到文件行）、WARN 确认；
  版本号自动预填（chart.yaml 版本未被占用则用之，否则 nextVersion），
  确认时同步写回 chart.yaml。
- 文件树：新建文件/目录/相位（问询 chart.yaml phases 声明）/上传，
  右键重命名/删除/撤销；保留三件套禁止改名删除；文件操作 Ctrl+Z 撤销
  （焦点不在编辑器时）。
- 新建流程：列表页弹窗（名称/版本/描述）→ `/apps/ide?new=1&name=…`
  脚手架（三件套 + hosts=应用名，默认打开 deploy.yaml）；保存成功后
  URL replace 成 `?app=<id>&base=<ver>`。旧 `/apps/:id/edit` 重定向兼容。

验收（内嵌二进制 + 浏览器实测）：模块补全（shel→shell）、`{{ .`根变量、
`{{ .app.`子级、ERROR 门禁（未知模块拦截+问题面板）、创建应用入库、
注释逐字回读、编辑模式版本推进（1.0.1 预填）、语义着色全通过。

首轮实测修掉的问题：
- 目录树一级目录逐字符嵌套（f→fi→fil→files）：ensureDir 对无斜杠目录
  的父节点算错（slice(0,-1)），一级目录父节点应为根。
- 行号槽整条红带：Monaco 主题 colors 必须是合法 #RRGGBB(AA)，无 # 的
  6 位值解析失败把 --vscode-editorGutter-background 污染成纯红。
- 中文全角标点被 unicodeHighlight 框成黄框：ambiguousCharacters 关闭。
- 左侧树与编辑区新增拖拽分隔条（170–460px）。
- 离开守卫弹框循环：会话失效（服务重启/过期）后自动暂存每 2.5s 收 401
  → 全局事件反复把路由推向登录页 → 离开守卫每次弹确认框（编辑中被
  循环打断）。修复：登录跳转不拦（强制登出拦不住）；已成功暂存的改动
  离开直接放行（轻提示可回来恢复）；401 后停自动暂存。实测：重启服务
  后继续编辑不再弹框、静默跳登录，重登回来草稿横幅可恢复全部内容。
- 登录页/退出后弹浏览器原生认证框：requireAuth 对所有未认证 GET 都回
  401+WWW-Authenticate: Basic（本意为 Prometheus 抓取提示），而浏览器
  fetch 收到该头就弹原生登录框——守卫的 /api/me 探测在登录页刷新/退出
  后必中。修复：前端所有 API 请求显式 Accept: application/json；后端
  仅对非 JSON 调用方（Prometheus/curl/浏览器直访）发质询头，
  TestBasicChallengeOnlyForNonJSON 锁定。
- 补全弹窗重复项：三个来源各出一套候选——自研 provider（带中文文档）、
  monaco-yaml schema 补全（同名键第二套 + additionalProperties 文档已有
  键建议）、word-based 文档词补全。修复：completion:false +
  wordBasedSuggestions 'off'，键/值/变量补全由 provider 独占；monaco-yaml
  仅保留语法/schema 校验与 hover。
- 主动离开与异常中断分流：离开守卫改为三按钮抉择框（暂存并离开/丢弃
  修改并离开/取消），去留决定在离开那一刻收掉；草稿带 mode 标记
  （auto=自动暂存 / deliberate=主动暂存），重进时 deliberate 且底本一致
  静默续上（不弹横幅），auto（刷新/崩溃/会话失效中断）才弹恢复横幅。
- 参数枚举值补全 + 模板自动闭合：值位置上下文带所属模块（键位判定
  复用），固定值域参数出枚举候选——file/service/systemd_unit/package/
  user 的 state、user.shell 来自解析器 switch 白名单；通用规则兜底：
  ParamDoc type=bool → true/false，type=mode → 常用权限位（裸输入插
  "0755" 带引号防 YAML 八进制解析，已输开引号则闭合引号）。
  {{ }} 叶子变量接受后自动补 ' }}'（未闭合时）；空格风格说明：
  {{.x}} 与 {{ .x }} 对 Go template 等价，不影响执行，跟随用户已输入
  风格。
- 新建文件/目录弹窗化：约定优先再自定义——文件给 _helpers.tpl /
  values.schema.json / tasks/main.yaml / envs/prod.yaml / templates/
  app.conf.tpl（各带用途说明与内容脚手架，创建即打开），目录给 files/
  templates/ tasks/ charts/ envs/ modules/；已有项置灰标"已有"。
- 目录树悬停操作按钮：文件行悬停显示 ✎重命名/✕删除（已删除显示 ↩撤销），
  替代右键菜单成为主入口（内嵌浏览器右键常调不出）；三件套与目录不显示。
- chart 引用统一 map 形态 + 文档补齐 + 裸 playbook 编辑器：
  `chart: <名>` 简写移除（报错给迁移提示），平级 values/values_from/
  hosts/phase 拒绝（配置集中 map 内）；`wdp module chart` 可查（伪模块
  文档，playbook.ChartRefParams/Example 单一来源，/api/modules 同源注入
  → 编辑器文档面板直接展示）；编辑器 chart 骨架改 map 形态、chart map
  内 name: 值补全子 chart 名。新增「Playbook 编辑器」入口（/playbook/new，
  应用管理菜单）：单文件创作 + 下载 + localStorage 草稿，补全同源（根级
  形态缺省 play，与相位文件缺省任务相反）。
- 相位文件裸任务形态 + hosts 语义重构：deploy/自定义相位可直接写任务
  （连续裸任务合并为一个隐式 play，与 play 形态可混用）；hosts 全面
  可选——CLI 缺省打 chart 同名组（零主机时点名提示补组或显式写
  hosts），web 由选择器决定，显式 hosts 的 play 在选择器范围内按台账
  组细分（FromHosts 按 group_names 建组），组存在但选择器无成员的
  play 跳过，模式完全未知（旧 chart 应用名写法）回退全集兼容。
  脚手架改裸任务形态；编辑器 ctx/decorate/schema 适配根级任务（含
  同级任务模块键泄漏进补全上下文的修复）。e2e：裸任务执行/分组细分/
  旧写法回退三场景。
- 补全骨架缩进两轮修复：(1) Monaco 插入多行 snippet 会把当前行行首缩进
  自动加到每个后续行，snippet 里带完整缩进会叠双层（参数行曾撑到 10
  空格）——snippet 内缩进改为相对行首深度；(2) 模块参数必须是模块键的
  子节点（比键深一层，与 get_url/url 示例层级一致），与键同列会被解析
  成任务 map 的第二个模块键——参数相对深度 = dash 行 4 / 续行 2。另修
  ctx 识别：tasks: 与列表项同缩进（仓库示例写法）时任务续行被误判
  play 级、出不了模块补全（父键判定 < 改 <=）。
- 部分保存清空底本三件套：只提交改动文件时 applySpec 把未提交的
  values.yaml 重写成 {}（模板渲染全炸）、deploy.yaml 报空。修复：未
  提交的三件套保留底本原文；chart.yaml 仅当底本版本号 == 请求版本时
  原样保留，升版本保存走生成路径写入新版本号。附带修 newTestServer
  缺省 DataDir 落相对路径导致的版本制品跨测试残留。

---

## 15. 编辑器数据一致性修复 + 补全文档化 + 下载/草稿箱整合

严格测试发现的三组静默数据问题（全部已修 + 回归测试锁定）：

- **重命名丢失/复制**：ChartFS.rename 直接把旧条目移出 Map，保存请求既无
  新路径也无旧路径删除——仅重命名时 isDirty=false「没有改动」、改内容保存
  后新旧文件并存（复制非移动）、草稿往返同样复制。修复：底本文件旧路径
  登记删除占位（进 delete_files），新路径 original='' 强制回传；树占位可
  「撤销删除」找回底本。
- **空内容文件静默丢弃/回滚**：后端 SpecFile.Content 用空串兼任「未提交」
  ——新建空文件不落盘、清空文件回滚成底本内容且保存成功（编辑器状态与
  制品永久漂移）。修复：Content 改 *string（nil=未提交/非nil含空串=显式
  提交）；清空 deploy/values 显式报错（校验面板 ERROR 阻断）。
- **校验失败绕过保存门禁**：doValidate 失败吞成 []，网络错误/500/越权被
  当「校验通过」放行保存。修复：失败返回 null，保存立即中止。
- 上限统一：save/validate 请求体 8MiB（decodeJSONLarge，与草稿同口径；
  此前全局 1MiB 造成「暂存成功、保存永久失败」死局）；前端上传单文件
  512KiB 与读侧 specTextLimit 对齐。
- 其它：本地草稿 key 加用户名隔离；BottomPanel/装饰定时器/异步挂载三处
  Monaco 泄漏；values.schema.json 编辑热重装配；保存描述替换 chart.yaml
  行（特殊字符安全引用）；上传落点残留；patchPhases flow 写法防重复键；
  new=1 缺名守卫；暂存失败 10s 重试 + 窗口焦点恢复会话探测；edit 类树
  操作可撤销。
- **lint 行号结构化**：model.Task.Line（yaml 节点行号，playbook 解析填
  充）→ LintIssue.Line → validateIssue.line，前端优先结构化字段、消息正则
  兜底；解析错误消息也带 line。
- **底本非最新 WARN**：编辑模式校验 base != latest 时提示并行编辑风险
  （版本化允许旧底本保存，是特性，但保存者应知情）。
- **版本制品下载**：GET /api/apps/{id}/download?version=（app:view，进
  审计，Content-Disposition <name>-<version>.tgz）；应用列表版本行「下载」
  按钮，可直接 `wdp apply` 复用。
- **补全文档化（新手向）**：所有补全候选带 documentation——值补全（src/
  include/hosts/notify/相位/子 chart 各自语义）、枚举值（参数 desc + 当前
  候选）、bool/mode（权限位逐个解释 + 引号原因）、模板函数全集中文说明
  （sprig 白名单 ~90 个 + 类别兜底）、变量按来源说明（values/register/
  循环变量/内置，register 附 .stdout/.rc 用法）、helper/include 用法。
- **草稿箱并入「新建应用」页**：IDE 新建/上传入口下方内嵌草稿列表（继续
  编辑/删除）；新建弹窗不再自动弹出（手动点「IDE 新建」）；移除侧边栏
  「草稿箱」菜单与页面，/apps/drafts 重定向 /apps/new。
- **测试体系**：前端引入 vitest（npm test，51 用例：fs 重命名/脏判定/
  序列化/草稿往返、ctx 补全上下文、vars 变量域、validate 行号解析）；
  后端新增 apps_spec_regress_test.go（空文件/清空/重命名/大 payload）与
  apps_download_test.go（下载/行号/底本 WARN）。

## 16. 补全排序调整 + 弃用字段清理（定版前不背兼容包袱）

- **补全排序按使用习惯**：模块参数补全此前按字母排序（copy 的 src 排
  末尾），改为后端 ParamDoc 声明序（即使用顺序）；copy 参数声明改为
  src→dest→content→mode…（与示例一致），file 改 path→state→src→mode…
  （link 场景 src 紧随 state）。声明序同时驱动补全候选、snippet 骨架
  （取前 4 参数）与文档表。
- **弃用字段全量清理**（项目未定版，不留兼容路径）：
  - `with_items`（loop 别名）：解析、文档表、编辑器徽标全删，只留 loop；
  - `pre_deploy`/`post_deploy`（install 词干别名）：NormalizeHook 删除
    （恒等）、lint 白名单去别名、plan/executor 裸比较，单一拼写
    pre_install/post_install（其余相位词干=相位名）；
  - file 模块 `dest` 别名（=path）：解析与文档删除，示例同步；
  - unarchive `remove`（已废弃参数）：删除；
  - 执行 API 请求级 `phase` 缺省（旧客户端兼容）：删除，相位纯行级；
  - spec API 旧字段 `values_yaml`/`deploy_yaml`（请求侧）与 `chart_meta`
    投影（旧表单编辑器）：删除——三件套一律 verbatim files 提交，
    chart.yaml 元数据经生成路径从底本合并（TestChartMetaVerbatimRoundtrip
    锁定）；PlaybookPage 存为应用同步改 files 形态；
  - 前端旧编辑器地址 `/apps/:id/edit` 重定向、`/apps/drafts` 重定向、
    loadFromDraft 旧格式草稿兜底：删除。
- 文档同步：05（loop）、07（file 软链示例 dest→path、unarchive remove、
  copy 参数表顺序）、08（hook 单一拼写说明）。

## 17. 可用性与可观测性改造（重启对账 / 执行隔离 / 准入 / 观测 / 单实例锁）

外部审计五组问题逐项核实后的落地（结论：两组属实已修，一组部分属实
已修，两组属实但属分阶段工程、给方案未动刀）：

- **启动对账（属实，已修）**：全仓确实只有 apps_run/exec 三处写 run
  终态，server 被 kill 后 queued/running 的 run 永久悬空。store 新增
  `ReconcileStaleRuns`（runs + 悬空 run_tasks 同事务收口），`web.New`
  启动时执行：全部标 failed + 「结果未知请核对后重发」——执行是至多
  一次语义，重启后真相是不知道，标成功才是错的。
- **断连杀远端进程（部分属实，已修）**：应用执行路径早已挂
  `background()` ctx（不受浏览器断连影响）；但 `/api/exec` 直连
  `r.Context()`——关标签页/网络抖动 → ctx 取消 → agent 对进程组
  SIGKILL，远端停在半完成。exec 改挂 server 生命周期 ctx，断连后
  结果仍完整落 run 记录（响应写往死连接无害）。
- **应用级准入（已落地）**：同应用并发执行会交错写 marker/状态——
  此前只有主机级 gate，两个选择器打同一应用照样并行。`ActiveRunsByApp`
  + 入口 409，反馈冲突方（应用[相位]（状态 · run #id · 用户））。
- **可观测（已落地）**：`/metrics`（Prometheus 文本：请求计数/耗时
  对数直方图/在途数/goroutine/heap/GC，手写格式零依赖）+
  `/debug/pprof/*`（标准库桥接），都挂 requireAuth + admin 门；
  `metricsMiddleware` 只在 Run 挂（测试直连 Handler 不经过）。观测
  端点按方法注册（无方法的 `/debug/pprof/` 与 SPA 兜底 `GET /` 在
  Go 1.22 mux 里冲突——曾 panic）。
- **单实例锁（已落地）**：`internal/datalock` 对数据目录 flock
  （`.wdp.lock`，0600，写 pid 排障）；`wdp server` 在 store.Open 前加
  锁，双开立即失败并指明另一实例；句柄存活至进程退出，崩溃由内核
  回收不留死锁。非 Unix 平台退化为标记文件。
- **读放大（属实，分阶段）**：RunsPage 5s/AppRunPage 2s/HostsPage 30s
  全量轮询 × 每个打开的控制台 = 持续全表读；后端 scrapeLoop 每分钟
  每主机一次（有界，by design）。当前内网规模无害；正解是 run 事件
  SSE（已在遗留清单）+ 列表 ETag/游标，随 SSE 一起做。
- **web 包分层（属实，分阶段）**：非测试 6894 行混传输/业务/后台任务
  确认；目标结构 httpsrv（路由/中间件/serde）← console（AppService/
  RunService…，签名不碰 ResponseWriter）← worker（probe/scrape/
  certrenew）。策略：新功能先落 console 层、存量按文件迁移（每文件
  一 PR、行为零变化），不做大爆炸重构。store.go 同步按域拆文件。

## 18. 分层落地（store 域拆分 / worker / console）+ 读放大修复（SSE）

按第 17 节方案执行，全程行为零变化（既有测试不改断言全过）：

- **store 按域拆分**：store.go 2231 行 → 基础设施（Open/migrate/tx，
  330 行）+ hosts/apps/runs/users/enroll/registry/drafts/audit/metrics
  九个域文件（最大 apps.go 478 行）。同包多文件零风险，引用面不变。
- **internal/worker（后台任务）**：Prober（探活循环）与 Monitor（指标
  采样/派生/告警评估 + Prometheus 文本解析）从 web 迁出；依赖注入
  （store/logger/Fetch/Probe 回调），mTLS scheme 选择留在 web 传输层。
  web 侧留类型别名（ProbeResult/parsePromText）与注入点 startProber/
  startMonitor；Monitor 实例在 New 构造（主机删除 Forget 差分快照）。
  采样/解析行为测试随迁 worker（含新增 Forget 用例）。
- **internal/console（领域服务）**：AppService（Store/DataDir/Logger）
  + SpecReq/ApplySpec/PrepareWorkspace/PackAndStore/ReadSpecFromDir 与
  chart 文件系统助手（SecureJoin/WriteChartFile/CopyDir/PackChart/
  FileSha256）+ AppNameRe/VersionRe 迁入；web 留 handler 壳与类型别名。
  分层约定自此生效：新功能先落 console（签名不碰 ResponseWriter），
  web 只做解码/权限/调用/审计/响应。
- **读放大修复（SSE）**：runHub 扇出总线（每订阅者 32 缓冲，notify
  非阻塞——慢消费者丢事件不反压执行路径）+ `GET /api/runs/stream`
  （text/event-stream，15s 心跳，断开即退订）；六个写点（排队失败/
  running/终态/apps_run 任务明细/exec 终态/exec 任务明细）落库后发窄
  事件（id/status/summary）。前端 RunsPage/AppRunPage 订阅事件即时刷
  新，轮询降为兜底（收到过事件 5s→30s / 2s→30s，递归 setTimeout 动态
  间隔）；EventSource 同源带 cookie、自动重连，重连间隙由兜底轮询补齐。
  全部控制台同开时：静默期每 30s 一轮全表读（此前每 2-5s 一轮/页）。

## 19. 文件级拆分 + console 化二期（RunService/ExecService）

- **web/server.go 拆分**（816 → 257 行）：构造与生命周期留 server.go，
  路由表 → routes.go、会话表 → session.go、鉴权/登录 → authmw.go、
  HTTP 工具 → httpx.go、主机 CRUD → hosts.go。同包多文件零行为变化。
- **agent/apply.go 拆分**（646 → 483+175）：plan 运行状态机
  （planRun/planManager）→ planrun.go；HTTP 端点与执行接线留 apply.go。
- **console.RunService**：runOneApp/markerValues/knownTargetNames/
  UnknownHostPattern/FirstHostValues 迁入；进度上报经 report.Reporter
  接口注入（web 的 dbReporter 落库+SSE），mTLS 探测留 web
  （runHostModels）。apps_run.go 494 → 275 行。
- **console.ExecService**：execOnHosts 迁入（HostModel 回调注入探活、
  notify 回调注入 SSE）；web/exec.go 留薄壳。ExecHostResult 类型别名
  保持 API 契约不变。
- **有意不抽**：inventory_ops 的薄 CRUD 与导入行处理（校验规则与 API
  行结构一一对应，抽层只换名不降耦）；enroll/sshinstall/upgrade 待
  与纳管生命周期一起考虑。原则重申：签名不碰 ResponseWriter 的领域
  动作才值得进 console。
- **golden 示例改打包形态**：examples/ 目录态示例改为 tgz 制品
  （docker 示例同轮被重写为在线/离线双模式简版）；plan golden 测试
  改走 tgz 加载路径（与「下载制品可直接 apply/导入」闭环同一条路），
  golden 按 UPDATE_GOLDEN 再生。

## 关键数据/接口速查

| 项 | 位置 |
|---|---|
| 模块元数据 | `GET /api/modules`（name/desc/params/example/free_form/rollback/readonly） |
| 编辑器结构元数据 | `GET /api/schema`（任务/play/chart 字段表 + 内置变量 + 模板函数白名单） |
| 编辑器整体校验 | `POST /api/apps/validate`（app_id>0 编辑模式 / 0=新建；200+issues，issue 带 line） |
| 版本制品下载 | `GET /api/apps/{id}/download?version=`（app:view，attachment `<name>-<version>.tgz`，进审计） |
| server 自监控 | `GET /metrics` · `GET /debug/pprof/*`（requireAuth + admin；中间件只在 Run 挂） |
| 数据目录锁 | `internal/datalock`（`.wdp.lock` flock；双开即拒） |
| 启动对账 | `web.New` → `ReconcileStaleRuns`（悬空 run 标 failed） |
| 编辑器草稿 | `GET/PUT/DELETE /api/apps/draft?key=<appID>` 或 `new:<名>`（表 app_drafts，按用户隔离） |
| 主机批量建档 | `POST /api/hosts/import` |
| 主机 facts / 指标 / 趋势 / 任务 | `GET /api/hosts/{id}/facts` · `/metrics[?format=json]` · `/series?metric=&labels=&hours=|from=` · `/tasks` |
| 告警 | `GET /api/alerts`；表 `host_alerts`（kind: cpu/mem/fs/offline, level: warn/crit） |
| agent 升级 | `POST /api/hosts/{id}/upgrade` · `POST /api/hosts/upgrade` |
| 审计 | `GET /api/audit?q=&limit=`；表 `audit_logs` |
| 指标存储 | `metrics_5m`（5 分钟桶 n/vsum/vmax，保留 30 天） |
| 版本注入 | `internal/buildinfo`（build.sh -X ldflags）；`/health` 上报 `build`、`bin_path` |

## 遗留与建议（未做）

- 多用户/RBAC/API Token：schema 未动，audit 已就绪（此前的差距分析定过
  三角色 admin/operator/viewer 起步的方案）。
- SSE 实时日志流、取消执行、定时执行、webhook 通知（差距分析第三层）。
- darwin agent 指标子集（无 CPU/内存使用率/load）——Linux 生产环境不受
  影响；若需要可走 host_statistics64。
- Sparkline 30d 视图跨月刻度只到 `MM-DD`，无年份（数据只有 30 天，够用）。
