package store

// 存量 SQLite 迁移线（v1..v17）。
//
// 为什么这批不与网络库共用：它们带着 SQLite 专有写法（INTEGER PRIMARY KEY
// AUTOINCREMENT、ALTER TABLE DROP COLUMN、CHECK(id = 1)、NOT NULL DEFAULT 加列），
// 而且**存量库必须逐条重放**——版本号与对象状态强相关，不能给已有库走基线
// 捷径。因此这批冻结为 SQLite 专用，新库（任意方言）也从 v1 逐条执行。
//
// 修改规则：本文件只增不改。历史迁移一旦被存量库执行过就不能再动，否则
// 「新库 schema」与「升级库 schema」会出现最难查的偏差。

var sqliteMigrations = []string{`
CREATE TABLE hosts (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT NOT NULL UNIQUE,
  address      TEXT NOT NULL,
  agent_port   INTEGER NOT NULL DEFAULT 7602,
  group_name   TEXT NOT NULL DEFAULT '',
  labels       TEXT NOT NULL DEFAULT '{}',
  status       TEXT NOT NULL DEFAULT 'unknown',
  last_seen_at TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TEXT NOT NULL
);
`, `
CREATE TABLE enroll_tokens (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  token         TEXT NOT NULL UNIQUE,
  host_name     TEXT NOT NULL DEFAULT '',
  agent_port    INTEGER NOT NULL DEFAULT 7602,
  created_at    TEXT NOT NULL,
  expires_at    TEXT NOT NULL,
  used_at       TEXT NOT NULL DEFAULT '',
  claim_host    TEXT NOT NULL DEFAULT '',
  claim_address TEXT NOT NULL DEFAULT ''
);
`, `
ALTER TABLE hosts ADD COLUMN pool TEXT NOT NULL DEFAULT '';
CREATE TABLE pools (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE host_groups (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE label_defs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  key        TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
`, `
-- 池/组多值化：单值列搬迁到关系表后删除
CREATE TABLE host_pools (host_id INTEGER NOT NULL, pool TEXT NOT NULL, UNIQUE(host_id, pool));
CREATE TABLE host_group_map (host_id INTEGER NOT NULL, group_name TEXT NOT NULL, UNIQUE(host_id, group_name));
INSERT INTO host_pools (host_id, pool) SELECT id, pool FROM hosts WHERE pool != '';
INSERT INTO host_group_map (host_id, group_name) SELECT id, group_name FROM hosts WHERE group_name != '';
ALTER TABLE hosts DROP COLUMN pool;
ALTER TABLE hosts DROP COLUMN group_name;
-- 应用（chart 版本化）
CREATE TABLE apps (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  name           TEXT NOT NULL UNIQUE,
  note           TEXT NOT NULL DEFAULT '',
  latest_version TEXT NOT NULL DEFAULT '',
  labels         TEXT NOT NULL DEFAULT '{}',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE TABLE app_pools (app_id INTEGER NOT NULL, pool TEXT NOT NULL, UNIQUE(app_id, pool));
CREATE TABLE app_groups (app_id INTEGER NOT NULL, group_name TEXT NOT NULL, UNIQUE(app_id, group_name));
CREATE TABLE app_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  app_id     INTEGER NOT NULL,
  version    TEXT NOT NULL,
  tgz_path   TEXT NOT NULL,
  sha256     TEXT NOT NULL DEFAULT '',
  size       INTEGER NOT NULL DEFAULT 0,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(app_id, version)
);
-- 执行记录（应用执行与远程命令共用；多应用顺序执行每个应用一行，seq 记序）
CREATE TABLE runs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT NOT NULL DEFAULT 'app',
  app_id     INTEGER NOT NULL DEFAULT 0,
  app_name   TEXT NOT NULL DEFAULT '',
  version    TEXT NOT NULL DEFAULT '',
  seq        INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL DEFAULT 'running',
  selector   TEXT NOT NULL DEFAULT '',
  summary    TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE run_tasks (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id  INTEGER NOT NULL,
  play    TEXT NOT NULL DEFAULT '',
  task    TEXT NOT NULL DEFAULT '',
  module  TEXT NOT NULL DEFAULT '',
  host    TEXT NOT NULL DEFAULT '',
  status  TEXT NOT NULL DEFAULT '',
  changed INTEGER NOT NULL DEFAULT 0,
  detail  TEXT NOT NULL DEFAULT ''
);
`, `
ALTER TABLE app_versions ADD COLUMN pools TEXT NOT NULL DEFAULT '';
ALTER TABLE app_versions ADD COLUMN groups TEXT NOT NULL DEFAULT '';
ALTER TABLE app_versions ADD COLUMN labels TEXT NOT NULL DEFAULT '';
`, `
ALTER TABLE runs ADD COLUMN user TEXT NOT NULL DEFAULT '';
CREATE TABLE audit_logs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user       TEXT NOT NULL DEFAULT '',
  action     TEXT NOT NULL, -- create / update / delete / import / install / login / logout / set_latest / upload
  object     TEXT NOT NULL, -- host / pool / group / label / app / version / run / chart
  name       TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT '',
  ip         TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX idx_audit_created ON audit_logs (id DESC);
`, `
CREATE TABLE metrics_5m (
  host_id INTEGER NOT NULL,
  metric  TEXT NOT NULL,
  labels  TEXT NOT NULL DEFAULT '',
  bucket  INTEGER NOT NULL, -- 5 分钟桶起点（Unix 秒）
  n       INTEGER NOT NULL DEFAULT 0,
  vsum    REAL NOT NULL DEFAULT 0,
  vmax    REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (host_id, metric, labels, bucket)
);
CREATE TABLE host_alerts (
  host_id    INTEGER NOT NULL,
  kind       TEXT NOT NULL, -- cpu / mem / fs / offline
  level      TEXT NOT NULL, -- warn / crit
  detail     TEXT NOT NULL DEFAULT '',
  value      REAL NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (host_id, kind)
);
`, `
ALTER TABLE app_versions ADD COLUMN phases TEXT NOT NULL DEFAULT ''; -- 相位清单（JSON 数组，版本创建时从 chart 提取；'' = 迁移前旧行）
ALTER TABLE runs ADD COLUMN phase TEXT NOT NULL DEFAULT ''; -- 本次执行使用的相位（app 类；exec 为空）
`, `
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT ''; -- admin / operator / viewer（空 = 迁移前旧行，按 operator 处理）
ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
CREATE TABLE user_scopes ( -- 细粒度授权：角色之外的按作用域追加授权（叠加模型）
  user_id INTEGER NOT NULL,
  verb    TEXT NOT NULL, -- 权限点，如 host:edit
  kind    TEXT NOT NULL DEFAULT '', -- '' = 全部 | pool | group | label
  value   TEXT NOT NULL DEFAULT '',
  UNIQUE(user_id, verb, kind, value)
);
`, `
CREATE TABLE app_drafts ( -- 编辑器草稿（按用户隔离；app_key = '<appID>' 或 'new:<应用名>'）
  user_id      INTEGER NOT NULL,
  app_key      TEXT NOT NULL,
  base_version TEXT NOT NULL DEFAULT '', -- 编辑底本（保存乐观锁回传）
  payload      TEXT NOT NULL,            -- 完整文件集 + UI 状态（JSON，前端定义）
  updated_at   TEXT NOT NULL,
  UNIQUE(user_id, app_key)
);
`, `
-- 明文通道显式声明：CA 启用时控制台→agent 默认走 mTLS（证书校验含主机名），
-- 只有明确标记的主机才允许明文 HTTP。此前"探活失败即回落明文"是可被中间人
-- 主动触发的降级（阻断 TLS 后自行应答即可拿到脚本/become 密码/制品）。
ALTER TABLE hosts ADD COLUMN allow_plaintext INTEGER NOT NULL DEFAULT 0;
-- 逐主机私钥交付标记：私钥只在"证书尚未交付"的窗口内可取，取走一次即
-- 作废该路径（纳管 token 会经 URL 进反代日志/shell 历史，仅靠 TTL 与
-- 来源 IP 绑定仍嫌宽）
ALTER TABLE enroll_tokens ADD COLUMN key_delivered_at TEXT NOT NULL DEFAULT '';
`, `
-- 热路径补索引（走新迁移版本，不改历史迁移）：
--   host_pools(pool) / host_group_map(group_name)：按池/组圈选主机
--   （ListHosts 的 scope 过滤）、池/组列表的成员计数与删除池/组时清理
--   成员关系，此前全部全表扫。host_id 一侧无需另建——建表时的
--   UNIQUE(host_id, pool/group_name) 前缀已覆盖按主机删/查。
--   runs(app_id, status) / runs(status)：执行互斥预检（同应用 queued/
--   running 判定）与启动期失败收尾按状态扫表；runs 随执行历史线性增长，
--   无索引时越用越慢。
CREATE INDEX idx_host_pools_pool ON host_pools (pool);
CREATE INDEX idx_host_group_map_group ON host_group_map (group_name);
CREATE INDEX idx_runs_app_status ON runs (app_id, status);
CREATE INDEX idx_runs_status ON runs (status);
`, `
-- 远程命令的执行证据：exec run 此前只记 selector/用户，事后无法回答
-- "谁在哪台机执行了什么命令"（应用执行有版本化 tgz 可回溯，exec 完全
-- 没有）。script 存截断快照（web 层 16 KiB），script_sha256 记完整脚本
-- 的哈希——截断不损害证据链（哈希可对质完整脚本），明文体积可控。
-- 应用执行两列为空串。
ALTER TABLE runs ADD COLUMN script TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN script_sha256 TEXT NOT NULL DEFAULT '';
-- run_tasks 按 run_id 取明细是详情页与 DeleteRun 的唯一访问路径
CREATE INDEX idx_run_tasks_run ON run_tasks (run_id);
`, `
-- CSR 纳管（docs/20）：token 一旦接受过 CSR 即锁定到该公钥——同钥幂等
-- 重试，异钥 410（token 泄露后换钥匙冒名）。key_delivered_at 是旧
-- 私钥交付流程的字段，列保留不重建表）。
ALTER TABLE enroll_tokens ADD COLUMN csr_pubkey_sha TEXT NOT NULL DEFAULT '';
`, `
-- 控制台运行时设置（docs/21 设置页）：全库仅这一份。
--   CHECK(id=1)      —— schema 层强制单行，物理上不存在第二份
--   version          —— 乐观锁：PUT 带期望版本，并发写冲突方 409
--   data             —— 设置文档 JSON（字段由 web 层定义与校验）
-- 启动时加载并覆盖 flag 默认（flag 退化为首次引导值）；变更即时生效。
CREATE TABLE settings (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  data       TEXT NOT NULL,
  version    INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by TEXT NOT NULL
);
`, `
-- agent 版本落库（升级门控）：探活拿到的 /health build 写进台账，
-- 主机列表据此显示「可升级/已是最新」——此前只进 facts 快照，列表页
-- 无从比较，升级按钮永远显示。（迁移只能追加：插在中间会让既有库的
-- 版本号漂移、重放后序迁移直接撞已存在对象）
ALTER TABLE hosts ADD COLUMN agent_build TEXT NOT NULL DEFAULT '';
`, `
-- agent 模块集与版本模块清单（分层方案 P2 能力对账）：探活顺带抓取
-- agent /info 的模块集落 hosts；应用版本创建时提取各相位使用的内置
-- 模块清单（相位 → 名单的 JSON 对象）。执行受理期两者对账，agent 缺
-- 模块在受理期报错，而不是执行到一半失败。'' = 未知（老 agent / 迁移
-- 前旧行），对账按放行处理不阻塞存量。
ALTER TABLE hosts ADD COLUMN agent_modules TEXT NOT NULL DEFAULT '';
ALTER TABLE app_versions ADD COLUMN modules TEXT NOT NULL DEFAULT '';
`}
