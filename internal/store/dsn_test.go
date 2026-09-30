package store

import (
	"fmt"
	"strings"
	"testing"
)

// envOf 构造固定环境变量映射（测试不依赖宿主环境）。
func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// TestParseDSNAddressForms 钉住 --db 的各种写法与方言识别。
// 向后兼容是硬要求：历史 --db 传的是 SQLite 文件路径，语义不能变。
func TestParseDSNAddressForms(t *testing.T) {
	cases := []struct {
		in       string
		dialect  string
		path     string // sqlite 路径断言
		host     string
		dbname   string
		params   map[string]string
		redacted string
	}{
		{in: "wdp.db", dialect: "sqlite", path: "wdp.db", redacted: "wdp.db"},
		{in: "/var/lib/wdp/wdp.db", dialect: "sqlite", path: "/var/lib/wdp/wdp.db", redacted: "/var/lib/wdp/wdp.db"},
		{in: ":memory:", dialect: "sqlite", path: ":memory:", redacted: ":memory:"},
		{in: "file:wdp.db?busy_timeout=10000", dialect: "sqlite", path: "wdp.db",
			params: map[string]string{"busy_timeout": "10000"}, redacted: "wdp.db?busy_timeout=10000"},
		{in: "sqlite:///var/lib/wdp/wdp.db", dialect: "sqlite", path: "/var/lib/wdp/wdp.db", redacted: "/var/lib/wdp/wdp.db"},
		{in: "postgres://wdp:s3cret@db.internal:5432/wdpdb", dialect: "postgres", host: "db.internal", dbname: "wdpdb",
			redacted: "postgres://wdp:***@db.internal:5432/wdpdb?sslmode=require"},
		{in: "postgresql://db.internal/wdpdb", dialect: "postgres", host: "db.internal", dbname: "wdpdb",
			redacted: "postgres://postgres@db.internal:5432/wdpdb?sslmode=require"},
		{in: "mysql://wdp:pw@127.0.0.1:3306/wdp", dialect: "mysql", host: "127.0.0.1", dbname: "wdp",
			redacted: "mysql://wdp:***@127.0.0.1:3306/wdp?charset=utf8mb4&collation=utf8mb4_general_ci&loc=UTC&parseTime=true"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			d, err := ParseDSN(tc.in, nil)
			if err != nil {
				t.Fatalf("ParseDSN(%q) 报错: %v", tc.in, err)
			}
			if d.Dialect.Name() != tc.dialect {
				t.Fatalf("方言 = %s, want %s", d.Dialect.Name(), tc.dialect)
			}
			if tc.path != "" && d.Config.Path != tc.path {
				t.Errorf("路径 = %q, want %q", d.Config.Path, tc.path)
			}
			if tc.host != "" && d.Config.Host != tc.host {
				t.Errorf("主机 = %q, want %q", d.Config.Host, tc.host)
			}
			if tc.dbname != "" && d.Config.DBName != tc.dbname {
				t.Errorf("库名 = %q, want %q", d.Config.DBName, tc.dbname)
			}
			for k, v := range tc.params {
				if d.Config.Params[k] != v {
					t.Errorf("参数 %s = %q, want %q", k, d.Config.Params[k], v)
				}
			}
			if tc.redacted != "" && d.Redacted != tc.redacted {
				t.Errorf("Redacted = %q, want %q", d.Redacted, tc.redacted)
			}
			// 硬要求：可外显的地址里不得出现密码
			if strings.Contains(d.Redacted, "s3cret") || strings.Contains(d.Redacted, ":pw@") {
				t.Errorf("Redacted 泄漏了密码: %q", d.Redacted)
			}
		})
	}
}

// TestDSNStringNeverLeaksPassword 任何打印 DSN 的路径都不得泄漏密码
// （String() 返回遮罩值，误用 %v 也不会漏）。
func TestDSNStringNeverLeaksPassword(t *testing.T) {
	d, err := ParseDSN("postgres://wdp:topsecret@db:5432/wdp", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 三个外显口都不得含密码：String()（%v/%s 打印）、Redacted、以及 fmt 包装
	for _, s := range []string{d.String(), d.Redacted, fmt.Sprint(d), fmt.Sprintf("%v", d)} {
		if strings.Contains(s, "topsecret") {
			t.Errorf("外显串泄漏密码: %q", s)
		}
	}
	// 反之，驱动串必须保留真密码（否则连不上）——它与外显串是两个口
	if !strings.Contains(d.DriverDSN(), "topsecret") {
		t.Error("DriverDSN 丢了密码，连接会失败")
	}
}

// TestParseDSNEnvInterpolation 密码可经 ${VAR} 注入，避免进命令行与 shell 历史。
func TestParseDSNEnvInterpolation(t *testing.T) {
	env := envOf(map[string]string{"WDP_DB_PASS": "p@ss:word/1", "PGHOST": "db.internal"})
	d, err := ParseDSN("postgres://wdp:${WDP_DB_PASS}@${PGHOST}:5432/wdp", env)
	if err != nil {
		t.Fatalf("插值解析失败: %v", err)
	}
	if d.Config.Password != "p@ss:word/1" {
		t.Errorf("密码 = %q, want %q", d.Config.Password, "p@ss:word/1")
	}
	if d.Config.Host != "db.internal" {
		t.Errorf("主机 = %q, want db.internal", d.Config.Host)
	}
	if strings.Contains(d.Redacted, "p@ss") || strings.Contains(d.Redacted, "word") {
		t.Errorf("遮罩串泄漏了插值后的密码: %q", d.Redacted)
	}

	// 也支持不带花括号的 $VAR
	d2, err := ParseDSN("mysql://wdp:$WDP_DB_PASS@127.0.0.1/wdp", env)
	if err != nil {
		t.Fatalf("$VAR 形式解析失败: %v", err)
	}
	if d2.Config.Password != "p@ss:word/1" {
		t.Errorf("$VAR 密码 = %q", d2.Config.Password)
	}
}

// TestParseDSNUndefinedEnvFails 未定义变量必须报错而不是展开为空
// （静默空密码表现为"认证失败"，排查成本高）。
func TestParseDSNUndefinedEnvFails(t *testing.T) {
	_, err := ParseDSN("postgres://wdp:${WDP_NO_SUCH_VAR}@db:5432/wdp", envOf(nil))
	if err == nil {
		t.Fatal("未定义环境变量应报错")
	}
	if !strings.Contains(err.Error(), "WDP_NO_SUCH_VAR") {
		t.Errorf("错误信息应点名缺失的变量: %v", err)
	}
}

// TestParseDSNNativeForms 两个驱动的原生连接串也能识别。
func TestParseDSNNativeForms(t *testing.T) {
	d, err := ParseDSN("host=db user=wdp password=pw dbname=wdpdb port=5433", nil)
	if err != nil {
		t.Fatalf("PG 原生串解析失败: %v", err)
	}
	if d.Dialect.Name() != "postgres" || d.Config.Host != "db" || d.Config.DBName != "wdpdb" || d.Config.Port != 5433 {
		t.Errorf("PG 原生串解析结果异常: %+v", d.Config)
	}
	if d.Config.Params["sslmode"] != "require" {
		t.Errorf("PG 原生串缺省 sslmode 应为 require，实际 %q", d.Config.Params["sslmode"])
	}
	if strings.Contains(d.Redacted, "pw") {
		t.Errorf("原生串遮罩失效: %q", d.Redacted)
	}

	d2, err := ParseDSN("wdp:pw@tcp(db:3306)/wdpdb", nil)
	if err != nil {
		t.Fatalf("MySQL 原生串解析失败: %v", err)
	}
	if d2.Dialect.Name() != "mysql" {
		t.Errorf("MySQL 原生串方言 = %s", d2.Dialect.Name())
	}
	if strings.Contains(d2.Redacted, "pw@") {
		t.Errorf("MySQL 原生串遮罩失效: %q", d2.Redacted)
	}
	if !strings.Contains(d2.DriverDSN(), "pw@") {
		t.Error("MySQL 原生串的驱动串应保留密码")
	}
}

// TestParseDSNPGSslmodeDefault 安全加工：PG 缺省要求 TLS，但允许显式放宽。
func TestParseDSNPGSslmodeDefault(t *testing.T) {
	d, err := ParseDSN("postgres://u@db/wdp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Config.Params["sslmode"]; got != "require" {
		t.Errorf("缺省 sslmode = %q, want require", got)
	}
	d2, err := ParseDSN("postgres://u@db/wdp?sslmode=disable", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := d2.Config.Params["sslmode"]; got != "disable" {
		t.Errorf("显式 sslmode 未被尊重: %q", got)
	}
}

// TestParseDSNErrors 明显的错配置要报错而不是静默退化成 SQLite 文件。
func TestParseDSNErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"postgres://",
		"mysql://",
		"postgres://u@db:5432/wdp?sslmode=require", // 合法，用于对照
	} {
		_, err := ParseDSN(in, envOf(nil))
		if in == "postgres://u@db:5432/wdp?sslmode=require" {
			if err != nil {
				t.Errorf("%q 应解析成功: %v", in, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q 应报错", in)
		}
	}
}
