package store

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"wdp/internal/store/dialect"
)

// 库地址（--db）解析。约定：
//
//	（空）                                  → SQLite，路径由调用方给缺省（<data>/wdp.db）
//	wdp.db / /var/lib/wdp/wdp.db / :memory: → SQLite 文件路径（不带 scheme 一律当文件，
//	                                          与历史 --db 语义完全兼容）
//	file:wdp.db?busy_timeout=10000          → SQLite（可带连接参数）
//	sqlite:///var/lib/wdp/wdp.db            → SQLite
//	postgres://user:pass@host:5432/wdp?sslmode=require
//	mysql://user:pass@host:3306/wdp?tls=true
//
// 安全加工（见 ParseDSN 与 DSN.Redacted）：
//   - 支持 ${VAR} / $VAR 环境变量插值：密码不进命令行、不进 shell 历史
//   - Display() 一律遮掉密码，可安全进日志/审计/错误信息
//   - PostgreSQL 缺省 sslmode=require（不是 disable）
const (
	envRedacted = "***"
)

// DSN 是解析后的库地址。
type DSN struct {
	// Dialect 目标方言。
	Dialect dialect.Dialect
	// Config 连接要素（SQLite 只用到 Path 与 Params）。
	Config dialect.Config
	// driverDSN 是驱动专用连接串（含密码，绝不外显）。
	driverDSN string
	// Redacted 是可安全外显的地址（密码已遮罩）。
	Redacted string
}

// Driver 返回 database/sql 驱动名。
func (d DSN) Driver() string { return d.Dialect.Driver() }

// DriverDSN 返回驱动专用连接串（含密码）。仅供 sql.Open 使用，不得写日志。
func (d DSN) DriverDSN() string { return d.driverDSN }

// String 实现 fmt.Stringer 时**返回遮罩后的地址**：这样任何 %v / %s 打印
// DSN 的地方都不会漏密码（误用也不会泄漏，这是有意的默认安全）。
func (d DSN) String() string { return d.Redacted }

// IsSQLite 报告是否为 SQLite（用于连接池与数据目录相关的分支）。
func (d DSN) IsSQLite() bool { return d.Dialect.Name() == "sqlite" }

// ParseDSN 解析库地址为方言 + 连接配置。
//
// env 为 nil 时取进程环境变量；测试可注入固定映射以断言插值行为。
func ParseDSN(raw string, env func(string) string) (DSN, error) {
	if env == nil {
		env = os.Getenv
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DSN{}, fmt.Errorf("empty database address")
	}
	// 环境变量插值先做：密码以 ${VAR} 形式给出时，后面的解析只看到展开值。
	expanded, err := expandEnv(raw, env)
	if err != nil {
		return DSN{}, err
	}

	switch {
	case strings.HasPrefix(expanded, "postgres://"), strings.HasPrefix(expanded, "postgresql://"):
		return parseNetworkDSN("postgres", expanded, env)
	case strings.HasPrefix(expanded, "mysql://"):
		return parseNetworkDSN("mysql", expanded, env)
	case strings.HasPrefix(expanded, "sqlite://"), strings.HasPrefix(expanded, "file:"):
		return parseSQLiteDSN(expanded)
	default:
		// 不带 scheme：原生 DSN 或 SQLite 文件路径。
		if d, ok, err := nativeDSN(expanded); ok || err != nil {
			return d, err
		}
		return parseSQLiteDSN(expanded)
	}
}

// nativeDSN 识别两个驱动各自的原生连接串写法（不是 URL 的那些）。
// 只有形态足够明确才认，避免把普通文件名误判成 DSN。
func nativeDSN(s string) (DSN, bool, error) {
	lower := strings.ToLower(s)
	// PG 的 keyword/value 形式：host=... user=... dbname=...
	if strings.Contains(lower, "host=") && strings.Contains(lower, "dbname=") {
		d, err := newDSN("postgres", dialect.Config{
			User:     kvField(s, "user"),
			Password: kvField(s, "password"),
			DBName:   kvField(s, "dbname"),
			Host:     kvField(s, "host"),
			Port:     atoiOr(kvField(s, "port"), 0),
			Params:   map[string]string{"sslmode": valueOr(kvField(s, "sslmode"), "require")},
		}, s)
		return d, true, err
	}
	// MySQL 原生 DSN：user:pass@tcp(host:port)/dbname
	if strings.Contains(s, "@tcp(") || strings.Contains(s, "@unix(") {
		d := DSN{
			Dialect:   mustDialect("mysql"),
			Config:    dialect.Config{},
			driverDSN: s,
			Redacted:  redactNative(s),
		}
		return d, true, nil
	}
	return DSN{}, false, nil
}

// parseNetworkDSN 解析 postgres:// 与 mysql:// 形式。
func parseNetworkDSN(name, raw string, env func(string) string) (DSN, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return DSN{}, fmt.Errorf("invalid %s database address: %w", name, err)
	}
	if u.User == nil && u.Host == "" {
		return DSN{}, fmt.Errorf("invalid %s database address: missing host", name)
	}
	cfg := dialect.Config{Params: map[string]string{}}
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Password = pw
		}
	}
	cfg.Host = u.Hostname()
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return DSN{}, fmt.Errorf("invalid port %q in database address", p)
		}
		cfg.Port = n
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			cfg.Params[k] = vs[0]
		}
	}
	return newDSN(name, cfg, raw)
}

// parseSQLiteDSN 解析 sqlite://、file:// 与裸文件路径。
func parseSQLiteDSN(raw string) (DSN, error) {
	path := raw
	params := map[string]string{}
	switch {
	case strings.HasPrefix(raw, "sqlite://"):
		rest := strings.TrimPrefix(raw, "sqlite://")
		// sqlite:///abs/path 与 sqlite://rel/path 都要支持
		p, q, _ := strings.Cut(rest, "?")
		path = p
		params = parseQuery(q)
	case strings.HasPrefix(raw, "file:"):
		rest := strings.TrimPrefix(raw, "file:")
		// file::memory: 与 file:path?params 两种形态
		p, q, _ := strings.Cut(rest, "?")
		if p == ":memory:" {
			path = ":memory:"
		} else {
			path = strings.TrimPrefix(p, "//")
		}
		params = parseQuery(q)
	default:
		// 裸路径：允许带 ? 参数（历史 --db 不带参数，这里向后兼容地扩展）
		if p, q, ok := strings.Cut(raw, "?"); ok {
			path = p
			params = parseQuery(q)
		}
	}
	if strings.TrimSpace(path) == "" {
		return DSN{}, fmt.Errorf("empty SQLite database path")
	}
	return newDSN("sqlite", dialect.Config{Path: path, Params: params}, raw)
}

// newDSN 组装 DSN（统一在此补缺省值并生成驱动串与遮罩串）。
//
// 缺省值必须在**这一处**补齐：Config、driverDSN、Redacted 三者必须描述同一个
// 连接，否则日志里显示的地址与实际连的库不一致（排查时最误导的一类问题）。
func newDSN(name string, cfg dialect.Config, raw string) (DSN, error) {
	d := mustDialect(name)
	if d == nil {
		return DSN{}, fmt.Errorf("unsupported database dialect %q", name)
	}
	cfg = applyDefaults(name, cfg)
	return DSN{
		Dialect:   d,
		Config:    cfg,
		driverDSN: d.DSN(cfg),
		Redacted:  redactAddress(name, cfg),
	}, nil
}

// applyDefaults 补齐各方言的连接缺省值。
//
// PostgreSQL 的 sslmode 缺省取 require：远程库不走明文（与参考实现刻意相反，
// 那里缺省 disable）。显式给值时不覆盖。
// MySQL 缺省 utf8mb4：旧参考用 utf8（等价 utf8mb3），存不下 emoji 与部分
// CJK 扩展字符，而 wdp 的 task 名/命令输出会带这类字符。
func applyDefaults(name string, cfg dialect.Config) dialect.Config {
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	switch name {
	case "sqlite":
		if cfg.Path == "" {
			cfg.Path = "wdp.db"
		}
	case "postgres":
		cfg.Host = valueOr(cfg.Host, "127.0.0.1")
		if cfg.Port == 0 {
			cfg.Port = 5432
		}
		cfg.User = valueOr(cfg.User, "postgres")
		cfg.DBName = valueOr(cfg.DBName, "postgres")
		cfg.Params["sslmode"] = valueOr(cfg.Params["sslmode"], "require")
	case "mysql":
		cfg.Host = valueOr(cfg.Host, "127.0.0.1")
		if cfg.Port == 0 {
			cfg.Port = 3306
		}
		cfg.User = valueOr(cfg.User, "root")
		cfg.DBName = valueOr(cfg.DBName, "wdp")
		cfg.Params["charset"] = valueOr(cfg.Params["charset"], "utf8mb4")
		cfg.Params["collation"] = valueOr(cfg.Params["collation"], "utf8mb4_general_ci")
		cfg.Params["parseTime"] = valueOr(cfg.Params["parseTime"], "true")
		cfg.Params["loc"] = valueOr(cfg.Params["loc"], "UTC")
	}
	return cfg
}

// redactAddress 生成可外显的地址：网络库遮密码，SQLite 显示文件路径。
// 入参是补齐缺省值之后的配置，因此显示的就是实际连接的地址。
func redactAddress(name string, cfg dialect.Config) string {
	switch name {
	case "sqlite":
		if len(cfg.Params) == 0 {
			return cfg.Path
		}
		return cfg.Path + "?" + encodeQuery(cfg.Params)
	default:
		host := cfg.Host
		if cfg.Port > 0 {
			host += ":" + strconv.Itoa(cfg.Port)
		}
		user := cfg.User
		if cfg.Password != "" {
			user += ":" + envRedacted + "@"
		} else if user != "" {
			user += "@"
		}
		s := name + "://" + user + host + "/" + cfg.DBName
		if len(cfg.Params) > 0 {
			s += "?" + encodeQuery(cfg.Params)
		}
		return s
	}
}

// redactNative 遮罩原生 DSN 里的密码（user:pass@tcp(...) / host=... password=...）。
func redactNative(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		if d, err := ParseDSN(s, os.Getenv); err == nil {
			return d.Redacted
		}
	}
	if at := strings.Index(s, "@"); at > 0 {
		if colon := strings.Index(s[:at], ":"); colon >= 0 {
			return s[:colon+1] + envRedacted + s[at:]
		}
	}
	return regexp.MustCompile(`(?i)password=\S+`).ReplaceAllString(s, "password="+envRedacted)
}

// envRefRe 匹配 ${VAR} 与 $VAR 两种插值写法。
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandEnv 展开库地址里的 ${VAR} / $VAR。
//
// 为什么要它：把密码写进 --db 会进 shell 历史与 ps 输出（同机可见）。
// 有了插值，运维可以写
//
//	--db 'postgres://wdp:${WDP_DB_PASS}@db:5432/wdp'
//
// 而密码只存在于环境变量里。未定义的变量**报错而不是展开为空**：静默空密码
// 会表现为"认证失败"，排查成本高且容易误改配置。
func expandEnv(s string, env func(string) string) (string, error) {
	var missing []string
	out := envRefRe.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.TrimPrefix(m, "$")
		name = strings.Trim(name, "{}")
		v := env(name)
		if v == "" {
			missing = append(missing, name)
			return m
		}
		// 值里可能含 URL 保留字符（@ : / ?）：按 URL 用户信息段转义，
		// 避免密码里的 @ 把 host 切错。
		return urlEncodeUserinfo(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("database address references undefined environment variable(s): %s",
			strings.Join(missing, ", "))
	}
	return out, nil
}

// urlEncodeUserinfo 转义用户信息段里的保留字符（保留 $ 等普通字符）。
func urlEncodeUserinfo(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// parseQuery 把 `a=1&b=2` 解析为参数表。
func parseQuery(q string) map[string]string {
	out := map[string]string{}
	if q == "" {
		return out
	}
	for _, kv := range strings.Split(q, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		if k == "" {
			continue
		}
		if dec, err := url.QueryUnescape(v); err == nil {
			v = dec
		}
		out[k] = v
	}
	return out
}

// encodeQuery 稳定编码参数表（键排序，便于比对与测试）。
func encodeQuery(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "&")
}

// sortStrings 插入排序（参数表很短，避免引 slices 只为这一处）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// mustDialect 取方言，未知返回 nil。
func mustDialect(name string) dialect.Dialect {
	d, err := dialect.Get(name)
	if err != nil {
		return nil
	}
	return d
}

// kvField 从 PG 的 keyword/value 串里取一个字段。
func kvField(s, key string) string {
	for _, f := range strings.Fields(s) {
		k, v, ok := strings.Cut(f, "=")
		if ok && strings.EqualFold(k, key) {
			return strings.Trim(v, "'\"")
		}
	}
	return ""
}

// atoiOr 解析整数，失败返回 def。
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// valueOr 取非空值，空则用 def。
func valueOr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
