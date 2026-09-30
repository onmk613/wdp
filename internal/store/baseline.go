package store

// 网络库建表基线。
//
// 来源与维护方式：由 `go run ./internal/store/tools/genbaseline -write` 从
// **跑完历史迁移的 SQLite schema** 自动导出，因此不存在「手抄漏列」的可能。
//
// 改动 schema 的正确做法：
//
//  1. 新增一条 portableMigrations（三库通用增量），**不要**直接改本文件；
//  2. 基线只在「网络库首次建库」时执行一次，历史库不会重放它。
//
// 只有当希望新库一步到位（不再逐条重放增量）时，才重新生成基线，并确认
// portableMigrations 里的等价增量对已有库仍然成立。
// internal/store/baseline_test.go 会断言基线与迁移产物结构一致。
var (
	postgresBaseline = []string{
		"CREATE TABLE app_drafts (\n  user_id BIGINT NOT NULL,\n  app_key TEXT NOT NULL,\n  base_version TEXT NOT NULL DEFAULT '',\n  payload TEXT NOT NULL,\n  updated_at TEXT NOT NULL,\n  UNIQUE(user_id, app_key)\n)",
		"CREATE TABLE app_groups (\n  app_id BIGINT NOT NULL,\n  group_name TEXT NOT NULL,\n  UNIQUE(app_id, group_name)\n)",
		"CREATE TABLE app_pools (\n  app_id BIGINT NOT NULL,\n  pool TEXT NOT NULL,\n  UNIQUE(app_id, pool)\n)",
		"CREATE TABLE app_versions (\n  id BIGSERIAL PRIMARY KEY,\n  app_id BIGINT NOT NULL,\n  version TEXT NOT NULL,\n  tgz_path TEXT NOT NULL,\n  sha256 TEXT NOT NULL DEFAULT '',\n  size BIGINT NOT NULL DEFAULT 0,\n  note TEXT NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL,\n  pools TEXT NOT NULL DEFAULT '',\n  groups TEXT NOT NULL DEFAULT '',\n  labels TEXT NOT NULL DEFAULT '',\n  phases TEXT NOT NULL DEFAULT '',\n  modules TEXT NOT NULL DEFAULT '',\n  UNIQUE(app_id, version)\n)",
		"CREATE TABLE apps (\n  id BIGSERIAL PRIMARY KEY,\n  name TEXT NOT NULL,\n  note TEXT NOT NULL DEFAULT '',\n  latest_version TEXT NOT NULL DEFAULT '',\n  labels TEXT NOT NULL DEFAULT '{}',\n  created_at TEXT NOT NULL,\n  updated_at TEXT NOT NULL\n)",
		"CREATE TABLE audit_logs (\n  id BIGSERIAL PRIMARY KEY,\n  user TEXT NOT NULL DEFAULT '',\n  action TEXT NOT NULL,\n  object TEXT NOT NULL,\n  name TEXT NOT NULL DEFAULT '',\n  detail TEXT NOT NULL DEFAULT '',\n  ip TEXT NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL\n)",
		"CREATE TABLE enroll_tokens (\n  id BIGSERIAL PRIMARY KEY,\n  token TEXT NOT NULL,\n  host_name TEXT NOT NULL DEFAULT '',\n  agent_port BIGINT NOT NULL DEFAULT 7602,\n  created_at TEXT NOT NULL,\n  expires_at TEXT NOT NULL,\n  used_at TEXT NOT NULL DEFAULT '',\n  claim_host TEXT NOT NULL DEFAULT '',\n  claim_address TEXT NOT NULL DEFAULT '',\n  key_delivered_at TEXT NOT NULL DEFAULT '',\n  csr_pubkey_sha TEXT NOT NULL DEFAULT ''\n)",
		"CREATE TABLE host_alerts (\n  host_id BIGINT NOT NULL PRIMARY KEY,\n  kind TEXT NOT NULL,\n  level TEXT NOT NULL,\n  detail TEXT NOT NULL DEFAULT '',\n  value DOUBLE PRECISION NOT NULL DEFAULT 0,\n  updated_at TEXT NOT NULL\n)",
		"CREATE TABLE host_group_map (\n  host_id BIGINT NOT NULL,\n  group_name TEXT NOT NULL,\n  UNIQUE(host_id, group_name)\n)",
		"CREATE TABLE host_groups (\n  id BIGSERIAL PRIMARY KEY,\n  name TEXT NOT NULL,\n  note TEXT NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL\n)",
		"CREATE TABLE host_pools (\n  host_id BIGINT NOT NULL,\n  pool TEXT NOT NULL,\n  UNIQUE(host_id, pool)\n)",
		"CREATE TABLE hosts (\n  id BIGSERIAL PRIMARY KEY,\n  name TEXT NOT NULL,\n  address TEXT NOT NULL,\n  agent_port BIGINT NOT NULL DEFAULT 7602,\n  labels TEXT NOT NULL DEFAULT '{}',\n  status TEXT NOT NULL DEFAULT 'unknown',\n  last_seen_at TEXT NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL,\n  updated_at TEXT NOT NULL,\n  allow_plaintext BIGINT NOT NULL DEFAULT 0,\n  agent_build TEXT NOT NULL DEFAULT '',\n  agent_modules TEXT NOT NULL DEFAULT ''\n)",
		"CREATE TABLE label_defs (\n  id BIGSERIAL PRIMARY KEY,\n  key TEXT NOT NULL,\n  note TEXT NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL\n)",
		"CREATE TABLE metrics_5m (\n  host_id BIGINT NOT NULL PRIMARY KEY,\n  metric TEXT NOT NULL,\n  labels TEXT NOT NULL DEFAULT '',\n  bucket BIGINT NOT NULL,\n  n BIGINT NOT NULL DEFAULT 0,\n  vsum DOUBLE PRECISION NOT NULL DEFAULT 0,\n  vmax DOUBLE PRECISION NOT NULL DEFAULT 0\n)",
		"CREATE TABLE pools (\n  id BIGSERIAL PRIMARY KEY,\n  name TEXT NOT NULL,\n  note TEXT NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL\n)",
		"CREATE TABLE run_tasks (\n  id BIGSERIAL PRIMARY KEY,\n  run_id BIGINT NOT NULL,\n  play TEXT NOT NULL DEFAULT '',\n  task TEXT NOT NULL DEFAULT '',\n  module TEXT NOT NULL DEFAULT '',\n  host TEXT NOT NULL DEFAULT '',\n  status TEXT NOT NULL DEFAULT '',\n  changed BIGINT NOT NULL DEFAULT 0,\n  detail TEXT NOT NULL DEFAULT ''\n)",
		"CREATE TABLE runs (\n  id BIGSERIAL PRIMARY KEY,\n  kind TEXT NOT NULL DEFAULT 'app',\n  app_id BIGINT NOT NULL DEFAULT 0,\n  app_name TEXT NOT NULL DEFAULT '',\n  version TEXT NOT NULL DEFAULT '',\n  seq BIGINT NOT NULL DEFAULT 0,\n  status TEXT NOT NULL DEFAULT 'running',\n  selector TEXT NOT NULL DEFAULT '',\n  summary TEXT NOT NULL DEFAULT '',\n  started_at TEXT NOT NULL,\n  finished_at TEXT NOT NULL DEFAULT '',\n  user TEXT NOT NULL DEFAULT '',\n  phase TEXT NOT NULL DEFAULT '',\n  script TEXT NOT NULL DEFAULT '',\n  script_sha256 TEXT NOT NULL DEFAULT ''\n)",
		"CREATE TABLE settings (\n  id BIGINT NOT NULL PRIMARY KEY,\n  data TEXT NOT NULL,\n  version BIGINT NOT NULL,\n  updated_at TEXT NOT NULL,\n  updated_by TEXT NOT NULL\n)",
		"CREATE TABLE user_scopes (\n  user_id BIGINT NOT NULL,\n  verb TEXT NOT NULL,\n  kind TEXT NOT NULL DEFAULT '',\n  value TEXT NOT NULL DEFAULT '',\n  UNIQUE(user_id, verb, kind, value)\n)",
		"CREATE TABLE users (\n  id BIGSERIAL PRIMARY KEY,\n  name TEXT NOT NULL,\n  password_hash TEXT NOT NULL,\n  created_at TEXT NOT NULL,\n  role TEXT NOT NULL DEFAULT '',\n  disabled BIGINT NOT NULL DEFAULT 0\n)",
		"CREATE INDEX idx_audit_created ON audit_logs (id DESC)",
		"CREATE INDEX idx_host_group_map_group ON host_group_map (group_name)",
		"CREATE INDEX idx_host_pools_pool ON host_pools (pool)",
		"CREATE INDEX idx_run_tasks_run ON run_tasks (run_id)",
		"CREATE INDEX idx_runs_app_status ON runs (app_id, status)",
		"CREATE INDEX idx_runs_status ON runs (status)",
	}

	mysqlBaseline = []string{
		"CREATE TABLE app_drafts (\n  user_id BIGINT NOT NULL,\n  app_key VARCHAR(255) NOT NULL,\n  base_version VARCHAR(255) NOT NULL DEFAULT '',\n  payload TEXT NOT NULL,\n  updated_at TEXT NOT NULL,\n  UNIQUE(user_id, app_key)\n)",
		"CREATE TABLE app_groups (\n  app_id BIGINT NOT NULL,\n  group_name VARCHAR(255) NOT NULL,\n  UNIQUE(app_id, group_name)\n)",
		"CREATE TABLE app_pools (\n  app_id BIGINT NOT NULL,\n  pool VARCHAR(255) NOT NULL,\n  UNIQUE(app_id, pool)\n)",
		"CREATE TABLE app_versions (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  app_id BIGINT NOT NULL,\n  version VARCHAR(255) NOT NULL,\n  tgz_path TEXT NOT NULL,\n  sha256 VARCHAR(255) NOT NULL DEFAULT '',\n  size BIGINT NOT NULL DEFAULT 0,\n  note VARCHAR(255) NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL,\n  pools VARCHAR(255) NOT NULL DEFAULT '',\n  groups VARCHAR(255) NOT NULL DEFAULT '',\n  labels VARCHAR(255) NOT NULL DEFAULT '',\n  phases VARCHAR(255) NOT NULL DEFAULT '',\n  modules VARCHAR(255) NOT NULL DEFAULT '',\n  UNIQUE(app_id, version)\n)",
		"CREATE TABLE apps (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  name TEXT NOT NULL,\n  note VARCHAR(255) NOT NULL DEFAULT '',\n  latest_version VARCHAR(255) NOT NULL DEFAULT '',\n  labels VARCHAR(255) NOT NULL DEFAULT '{}',\n  created_at TEXT NOT NULL,\n  updated_at TEXT NOT NULL\n)",
		"CREATE TABLE audit_logs (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  `user` VARCHAR(255) NOT NULL DEFAULT '',\n  action TEXT NOT NULL,\n  object TEXT NOT NULL,\n  name VARCHAR(255) NOT NULL DEFAULT '',\n  detail VARCHAR(255) NOT NULL DEFAULT '',\n  ip VARCHAR(255) NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL\n)",
		"CREATE TABLE enroll_tokens (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  token TEXT NOT NULL,\n  host_name VARCHAR(255) NOT NULL DEFAULT '',\n  agent_port BIGINT NOT NULL DEFAULT 7602,\n  created_at TEXT NOT NULL,\n  expires_at TEXT NOT NULL,\n  used_at VARCHAR(255) NOT NULL DEFAULT '',\n  claim_host VARCHAR(255) NOT NULL DEFAULT '',\n  claim_address VARCHAR(255) NOT NULL DEFAULT '',\n  key_delivered_at VARCHAR(255) NOT NULL DEFAULT '',\n  csr_pubkey_sha VARCHAR(255) NOT NULL DEFAULT ''\n)",
		"CREATE TABLE host_alerts (\n  host_id BIGINT NOT NULL PRIMARY KEY,\n  kind TEXT NOT NULL,\n  `level` TEXT NOT NULL,\n  detail VARCHAR(255) NOT NULL DEFAULT '',\n  `value` DOUBLE NOT NULL DEFAULT 0,\n  updated_at TEXT NOT NULL\n)",
		"CREATE TABLE host_group_map (\n  host_id BIGINT NOT NULL,\n  group_name VARCHAR(255) NOT NULL,\n  UNIQUE(host_id, group_name)\n)",
		"CREATE TABLE host_groups (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  name TEXT NOT NULL,\n  note VARCHAR(255) NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL\n)",
		"CREATE TABLE host_pools (\n  host_id BIGINT NOT NULL,\n  pool VARCHAR(255) NOT NULL,\n  UNIQUE(host_id, pool)\n)",
		"CREATE TABLE hosts (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  name TEXT NOT NULL,\n  address TEXT NOT NULL,\n  agent_port BIGINT NOT NULL DEFAULT 7602,\n  labels VARCHAR(255) NOT NULL DEFAULT '{}',\n  `status` VARCHAR(255) NOT NULL DEFAULT 'unknown',\n  last_seen_at VARCHAR(255) NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL,\n  updated_at TEXT NOT NULL,\n  allow_plaintext BIGINT NOT NULL DEFAULT 0,\n  agent_build VARCHAR(255) NOT NULL DEFAULT '',\n  agent_modules VARCHAR(255) NOT NULL DEFAULT ''\n)",
		"CREATE TABLE label_defs (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  `key` TEXT NOT NULL,\n  note VARCHAR(255) NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL\n)",
		"CREATE TABLE metrics_5m (\n  host_id BIGINT NOT NULL PRIMARY KEY,\n  metric TEXT NOT NULL,\n  labels VARCHAR(255) NOT NULL DEFAULT '',\n  bucket BIGINT NOT NULL,\n  n BIGINT NOT NULL DEFAULT 0,\n  vsum DOUBLE NOT NULL DEFAULT 0,\n  vmax DOUBLE NOT NULL DEFAULT 0\n)",
		"CREATE TABLE pools (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  name TEXT NOT NULL,\n  note VARCHAR(255) NOT NULL DEFAULT '',\n  created_at TEXT NOT NULL\n)",
		"CREATE TABLE run_tasks (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  run_id BIGINT NOT NULL,\n  play VARCHAR(255) NOT NULL DEFAULT '',\n  task VARCHAR(255) NOT NULL DEFAULT '',\n  module VARCHAR(255) NOT NULL DEFAULT '',\n  host VARCHAR(255) NOT NULL DEFAULT '',\n  `status` VARCHAR(255) NOT NULL DEFAULT '',\n  changed BIGINT NOT NULL DEFAULT 0,\n  detail VARCHAR(255) NOT NULL DEFAULT ''\n)",
		"CREATE TABLE runs (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  kind VARCHAR(255) NOT NULL DEFAULT 'app',\n  app_id BIGINT NOT NULL DEFAULT 0,\n  app_name VARCHAR(255) NOT NULL DEFAULT '',\n  version VARCHAR(255) NOT NULL DEFAULT '',\n  seq BIGINT NOT NULL DEFAULT 0,\n  `status` VARCHAR(255) NOT NULL DEFAULT 'running',\n  selector VARCHAR(255) NOT NULL DEFAULT '',\n  summary VARCHAR(255) NOT NULL DEFAULT '',\n  started_at TEXT NOT NULL,\n  finished_at VARCHAR(255) NOT NULL DEFAULT '',\n  `user` VARCHAR(255) NOT NULL DEFAULT '',\n  phase VARCHAR(255) NOT NULL DEFAULT '',\n  script VARCHAR(255) NOT NULL DEFAULT '',\n  script_sha256 VARCHAR(255) NOT NULL DEFAULT ''\n)",
		"CREATE TABLE settings (\n  id BIGINT NOT NULL PRIMARY KEY,\n  data TEXT NOT NULL,\n  version BIGINT NOT NULL,\n  updated_at TEXT NOT NULL,\n  updated_by TEXT NOT NULL\n)",
		"CREATE TABLE user_scopes (\n  user_id BIGINT NOT NULL,\n  verb VARCHAR(255) NOT NULL,\n  kind VARCHAR(255) NOT NULL DEFAULT '',\n  `value` VARCHAR(255) NOT NULL DEFAULT '',\n  UNIQUE(user_id, verb, kind, value)\n)",
		"CREATE TABLE users (\n  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,\n  name TEXT NOT NULL,\n  password_hash TEXT NOT NULL,\n  created_at TEXT NOT NULL,\n  role VARCHAR(255) NOT NULL DEFAULT '',\n  disabled BIGINT NOT NULL DEFAULT 0\n)",
		"CREATE INDEX idx_audit_created ON audit_logs (id DESC)",
		"CREATE INDEX idx_host_group_map_group ON host_group_map (group_name)",
		"CREATE INDEX idx_host_pools_pool ON host_pools (pool)",
		"CREATE INDEX idx_run_tasks_run ON run_tasks (run_id)",
		"CREATE INDEX idx_runs_app_status ON runs (app_id, status)",
		"CREATE INDEX idx_runs_status ON runs (status)",
	}
)
