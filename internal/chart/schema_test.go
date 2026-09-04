package chart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSchemaChart 写出带 values.schema.json 的 chart（files 相对路径 → 内容）。
func writeSchemaChart(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const testSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "app": {
      "type": "object",
      "properties": {
        "port": {"type": "integer", "minimum": 1, "maximum": 65535},
        "name": {"type": "string", "minLength": 1},
        "mode": {"enum": ["standalone", "cluster"]}
      },
      "required": ["name"]
    }
  }
}`

func schemaChartFiles(schema, values string, extra map[string]string) map[string]string {
	files := map[string]string{
		"chart.yaml":         "name: myapp\nversion: 1.0.0\n",
		"values.yaml":        values,
		"deploy.yaml":        "- hosts: all\n  tasks:\n    - shell: 'echo hi'\n",
		"values.schema.json": schema,
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

func TestSchemaValidate(t *testing.T) {
	c, err := Load(writeSchemaChart(t, schemaChartFiles(testSchema, "app: {name: demo, port: 8080, mode: cluster}\n", nil)))
	if err != nil {
		t.Fatal(err)
	}
	// 合法 values 通过
	if err := c.ValidateValuesSchema(map[string]any{
		"app": map[string]any{"name": "demo", "port": int64(8080), "mode": "cluster"},
	}); err != nil {
		t.Fatalf("合法 values 不应报错: %v", err)
	}
	// 类型错误：port 是字符串
	if err := c.ValidateValuesSchema(map[string]any{
		"app": map[string]any{"name": "demo", "port": "8080"},
	}); err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("类型错误应报 port: %v", err)
	}
	// 取值越界：port 超上限
	if err := c.ValidateValuesSchema(map[string]any{
		"app": map[string]any{"name": "demo", "port": int64(70000)},
	}); err == nil {
		t.Fatal("越界值应报错")
	}
	// 枚举外取值
	if err := c.ValidateValuesSchema(map[string]any{
		"app": map[string]any{"name": "demo", "mode": "swarm"},
	}); err == nil {
		t.Fatal("枚举外取值应报错")
	}
	// 必填缺失（JSON Schema required，比 chart.yaml required 更细粒度）
	if err := c.ValidateValuesSchema(map[string]any{"app": map[string]any{}}); err == nil {
		t.Fatal("缺 required 属性应报错")
	}
}

func TestSchemaInvalidFailsLoad(t *testing.T) {
	for _, bad := range []string{
		`{not json`,           // JSON 语法错误
		`{"type": "unknown"}`, // 非法 schema 关键字取值
	} {
		dir := writeSchemaChart(t, schemaChartFiles(bad, "app: {name: demo}\n", nil))
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), SchemaFile) {
			t.Fatalf("坏 schema 应在加载期报 %s: %v", SchemaFile, err)
		}
	}
}

func TestSchemaAbsentPasses(t *testing.T) {
	files := schemaChartFiles("", "app: {name: demo}\n", nil)
	delete(files, "values.schema.json")
	c, err := Load(writeSchemaChart(t, files))
	if err != nil {
		t.Fatal(err)
	}
	// 无 schema：任意 values 直接通过（含与 testSchema 同域的坏值）
	if err := c.ValidateValuesSchema(map[string]any{"app": map[string]any{"port": "8080"}}); err != nil {
		t.Fatalf("无 schema 不应报错: %v", err)
	}
}

func TestSubchartSchemaWalk(t *testing.T) {
	// 父 values 的 jdk 子树类型错误（version 应为 string 却是 int）由静态走查抓住
	files := schemaChartFiles("", "app: {name: demo}\njdk:\n  version: 17\n", map[string]string{
		"charts/jdk/chart.yaml":         "name: jdk\nversion: 1.0.0\n",
		"charts/jdk/values.yaml":        "version: \"11\"\n",
		"charts/jdk/deploy.yaml":        "- hosts: all\n  tasks:\n    - shell: 'echo {{ .version }}'\n",
		"charts/jdk/values.schema.json": `{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "properties": {"version": {"type": "string"}}}`,
	})
	delete(files, "values.schema.json") // 根 chart 无 schema，仅子 chart 自带
	c, err := Load(writeSchemaChart(t, files))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"app": map[string]any{"name": "demo"},
		"jdk": map[string]any{"version": int64(17)},
	}
	if err := c.ValidateSubchartsSchema(values); err == nil ||
		!strings.Contains(err.Error(), "subchart jdk") || !strings.Contains(err.Error(), "version") {
		t.Fatalf("父子树类型错误应被子 chart schema 抓住: %v", err)
	}
	// 正确类型通过（global 注入子作用域不干扰未设 additionalProperties 的 schema）
	ok := map[string]any{
		"app":    map[string]any{"name": "demo"},
		"jdk":    map[string]any{"version": "17"},
		"global": map[string]any{"env": "prod"},
	}
	if err := c.ValidateSubchartsSchema(ok); err != nil {
		t.Fatalf("合法子作用域不应报错: %v", err)
	}
}

func TestLintSchemaIssues(t *testing.T) {
	// lint 用合并 values 校验根 schema；envs/ 叠加默认值后同样校验
	files := schemaChartFiles(testSchema, "app: {name: demo, port: 8080}\n", map[string]string{
		"envs/prod.yaml": "app: {port: \"9090\"}\n", // 环境文件把 port 写成字符串
	})
	c, err := Load(writeSchemaChart(t, files))
	if err != nil {
		t.Fatal(err)
	}
	// 合并 values 本身合法（port 仍是 int）→ 只有 envs 叠加报错
	issues := Lint(c, map[string]any{"app": map[string]any{"name": "demo", "port": int64(8080)}})
	var envIssue bool
	for _, i := range issues {
		if i.Level == ERROR && i.Path == "envs/prod.yaml" && strings.Contains(i.Msg, "port") {
			envIssue = true
		}
	}
	if !envIssue {
		t.Fatalf("应发现 envs/prod.yaml 叠加后的类型错误: %v", issues)
	}

	// 合并 values 本身违约 → ERROR 落在 values.schema.json
	issues = Lint(c, map[string]any{"app": map[string]any{"name": "demo", "port": "8080"}})
	var rootIssue bool
	for _, i := range issues {
		if i.Level == ERROR && i.Path == SchemaFile && strings.Contains(i.Msg, "port") {
			rootIssue = true
		}
	}
	if !rootIssue {
		t.Fatalf("应发现合并 values 的类型错误: %v", issues)
	}
}
