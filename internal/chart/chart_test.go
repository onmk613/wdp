package chart

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeChart 在临时目录写出测试 chart（父 + 子 jdk）。
func writeChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("chart.yaml", "name: myapp\nversion: 0.1.0\ndescription: demo\n")
	mustWrite("values.yaml", `
app:
  name: demo
  port: 8080
  replicas: 1
global:
  env: dev
jdk:
  version: "17"
`)
	mustWrite("_helpers.tpl", `{{ define "app.fullname" }}{{ .app.name }}-{{ .global.env }}{{ end }}`)
	mustWrite("deploy.yaml", `
- name: 部署
  hosts: all
  tasks:
    - name: 安装 JDK
      chart: jdk
    - name: 配置
      template:
        src: templates/app.conf.tpl
        dest: /tmp/{{ .app.name }}.conf
`)
	mustWrite("templates/app.conf.tpl", "fullname={{ include \"app.fullname\" . }}\nport={{ .app.port }}\n")
	mustWrite("envs/prod.yaml", `
app:
  replicas: 3
  port: 9090
global:
  env: prod
`)
	// 子 chart jdk
	mustWrite("charts/jdk/chart.yaml", "name: jdk\nversion: 1.0.0\n")
	mustWrite("charts/jdk/values.yaml", "version: \"11\"\nhome: /opt/jdk\n")
	mustWrite("charts/jdk/deploy.yaml", `
- hosts: all
  tasks:
    - name: 输出版本
      shell: echo jdk-{{ .version }}
      register: jdk_out
    - name: 输出环境
      shell: echo env-{{ .global.env }}
  handlers:
    - name: jdk-reload
      shell: echo reload
`)
	return dir
}

func TestLoadChart(t *testing.T) {
	dir := writeChart(t)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Meta.Name != "myapp" || c.Meta.Version != "0.1.0" {
		t.Fatalf("meta: %+v", c.Meta)
	}
	if c.Values["app"] == nil || c.Values["global"] == nil {
		t.Fatalf("values: %#v", c.Values)
	}
	if len(c.Deploy) != 1 || len(c.Deploy[0].Tasks) != 2 {
		t.Fatalf("deploy: %+v", c.Deploy)
	}
	sub, ok := c.Subs["jdk"]
	if !ok {
		t.Fatal("缺少子 chart jdk")
	}
	if sub.Values["version"] != "11" {
		t.Fatalf("子 chart values: %#v", sub.Values)
	}
	if len(sub.Deploy[0].Handlers) != 1 {
		t.Fatal("子 chart handlers 解析失败")
	}
	if c.FindSub("jdk") == nil || c.FindSub("nope") != nil {
		t.Fatal("FindSub 异常")
	}
	if helpers := c.CollectHelpers(); !strings.Contains(helpers, "app.fullname") {
		t.Fatal("CollectHelpers 为空")
	}
	if files := c.EnvFiles(); len(files) != 1 || files[0] != "prod.yaml" {
		t.Fatalf("EnvFiles: %v", files)
	}
}

func TestBuildValues(t *testing.T) {
	dir := writeChart(t)
	c, _ := Load(dir)

	// 无覆盖
	v, err := c.BuildValues(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := v["app"].(map[string]any)
	if app["port"] != 8080 || app["replicas"] != 1 {
		t.Fatalf("默认 values: %#v", app)
	}

	// -f 覆盖 + --set 点路径
	v, err = c.BuildValues([]string{filepath.Join(dir, "envs", "prod.yaml")}, []string{"app.replicas=5"})
	if err != nil {
		t.Fatal(err)
	}
	app = v["app"].(map[string]any)
	if app["port"] != 9090 {
		t.Fatalf("-f 覆盖失败: %#v", app)
	}
	if app["replicas"] != int64(5) { // --set 覆盖 -f（推断为 int64）
		t.Fatalf("--set 覆盖失败: %#v", app)
	}
	if app["name"] != "demo" { // 未覆盖项保留
		t.Fatalf("深合并保留失败: %#v", app)
	}
	if v["global"].(map[string]any)["env"] != "prod" {
		t.Fatalf("global 覆盖失败: %#v", v["global"])
	}
}

// TestTgzRoundTrip 手工打 tgz 后加载，验证解包与顶层目录定位。
func TestTgzRoundTrip(t *testing.T) {
	dir := writeChart(t)
	tgz := filepath.Join(t.TempDir(), "myapp-0.1.0.tgz")
	if err := tarDir(dir, "myapp", tgz); err != nil {
		t.Fatal(err)
	}
	c, err := Load(tgz)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Meta.Name != "myapp" {
		t.Fatalf("tgz 加载: %+v", c.Meta)
	}
	if _, ok := c.Subs["jdk"]; !ok {
		t.Fatal("tgz 中缺少子 chart")
	}
}

// tarDir 把源目录打成 <prefix>/… 的 tgz。
func tarDir(src, prefix, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == src {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		name := prefix + "/" + filepath.ToSlash(rel)
		info, _ := d.Info()
		hdr := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size()}
		if d.IsDir() {
			hdr.Typeflag = tar.TypeDir
			return tw.WriteHeader(hdr)
		}
		hdr.Typeflag = tar.TypeReg
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
}
