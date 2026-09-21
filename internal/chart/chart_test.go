package chart

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
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
      chart: {name: jdk}
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
	if sub, err := c.FindSub("jdk"); err != nil || sub == nil {
		t.Fatal("FindSub 异常")
	}
	if _, err := c.FindSub("nope"); err == nil {
		t.Fatal("FindSub 未知名应报错")
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

// TestTgzTraversalContained 含 .. 穿越条目的 tgz 解包必须收敛在解包根内:
// 条目被提取到解包根内部而非越出, chart 本身不受影响正常加载。
func TestTgzTraversalContained(t *testing.T) {
	tgz := filepath.Join(t.TempDir(), "evil-0.1.0.tgz")
	f, err := os.Create(tgz)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	members := []struct{ name, body string }{
		{"chart.yaml", "name: demo\nversion: 0.1.0\ndescription: evil demo\n"},
		{"deploy.yaml", "- name: t\n  hosts: all\n  tasks: []\n"},
		{"../evil.txt", "evil"}, // 穿越条目: 须被收敛为解包根内路径
	}
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	c, err := Load(tgz)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Meta.Name != "demo" {
		t.Fatalf("chart 加载失败: %+v", c.Meta)
	}
	// 穿越条目被 securejoin 收敛提取到解包根内
	if got, err := os.ReadFile(filepath.Join(c.tmpDir, "evil.txt")); err != nil || string(got) != "evil" {
		t.Errorf("穿越条目应被收敛提取在解包根内: %v", err)
	}
	// 解包根之外 (tgz 所在目录与系统临时目录根) 不应出现穿越产物
	for _, outside := range []string{
		filepath.Join(filepath.Dir(tgz), "evil.txt"),
		filepath.Join(os.TempDir(), "evil.txt"),
	} {
		if _, err := os.Stat(outside); err == nil {
			t.Errorf("穿越条目越出了解包根: %s", outside)
		}
	}
}

// TestTgzExtractSizeCap 声明尺寸超出解包总量上限的 tgz 被拒绝（解压炸弹防御）：
// 封顶检查在读正文前触发, 无需真实写出超大内容。
func TestTgzExtractSizeCap(t *testing.T) {
	tgz := filepath.Join(t.TempDir(), "bomb-0.1.0.tgz")
	f, err := os.Create(tgz)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "chart.yaml", Mode: 0o644, Size: maxExtractBytes + 1}); err != nil {
		t.Fatal(err)
	}
	// 不写正文即关闭: tar 写入器会报缺正文错误, 无关紧要——
	// 头块已入流, 封顶检查在读取正文之前触发
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	// 断言用上限数字（语言无关）：中英文案均含该值
	if _, err := Load(tgz); err == nil || !strings.Contains(err.Error(), "2147483648") {
		t.Fatalf("应拒绝超限包, got %v", err)
	}
}

// TestPackageRoundTripWithSymlink Package 打包符号链接、Load 解包还原：
// 加载侧（tgz.go）支持 TypeSymlink 条目，打包侧静默跳过链接会造成
// "目录能跑、打包后缺文件"。
func TestPackageRoundTripWithSymlink(t *testing.T) {
	dir := writeChart(t)
	link := filepath.Join(dir, "templates", "link.conf.tpl")
	if err := os.Symlink("app.conf.tpl", link); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}
	out, err := Package(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out) != "myapp-0.1.0.tgz" {
		t.Fatalf("产物名: %s", out)
	}
	c, err := Load(out)
	if err != nil {
		t.Fatalf("打包产物应可加载: %v", err)
	}
	defer c.Close()
	got := filepath.Join(c.Dir, "templates", "link.conf.tpl")
	fi, err := os.Lstat(got)
	if err != nil {
		t.Fatalf("解包后缺少符号链接: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("应是符号链接: %v", fi.Mode())
	}
	if target, err := os.Readlink(got); err != nil || target != "app.conf.tpl" {
		t.Fatalf("链接目标: %q %v", target, err)
	}
	// 链接可解析到真实内容
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("符号链接应可解析: %v", err)
	}
	if !strings.Contains(string(data), "port={{ .app.port }}") {
		t.Fatalf("链接解析内容异常: %q", data)
	}
}

// TestLoadDirChartsNotDirError charts/ 存在但不可读（如被同名普通文件占位）
// 必须报错：静默跳过会让子 chart 难以定位地"消失"。charts/ 不存在仍正常。
func TestLoadDirChartsNotDirError(t *testing.T) {
	other := t.TempDir()
	for rel, content := range map[string]string{
		"chart.yaml":  "name: x\nversion: 1.0.0\n",
		"deploy.yaml": "- hosts: all\n  tasks: [{debug: {msg: x}}]\n",
	} {
		if err := os.WriteFile(filepath.Join(other, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 用普通文件占位 charts/ 目录本身：ReadDir 报 ENOTDIR（非 IsNotExist）
	if err := os.WriteFile(filepath.Join(other, "charts"), []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(other); err == nil || !strings.Contains(err.Error(), "charts") {
		t.Fatalf("charts/ 不可读应报错: %v", err)
	}
	// charts/ 不存在：可选目录，正常加载
	plain := t.TempDir()
	for rel, content := range map[string]string{
		"chart.yaml":  "name: x\nversion: 1.0.0\n",
		"deploy.yaml": "- hosts: all\n  tasks: [{debug: {msg: x}}]\n",
	} {
		if err := os.WriteFile(filepath.Join(plain, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := Load(plain); err != nil || len(c.Subs) != 0 {
		t.Fatalf("无 charts/ 目录应正常加载: %v", err)
	}
}

// errWriter 允许写满前 limit 字节，之后一律报错：模拟磁盘满等底层写失败。
type errWriter struct {
	limit   int
	written int
}

var _ io.Writer = (*errWriter)(nil)
var _ io.Writer = (*countWriter)(nil)

func (w *errWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.limit {
		return 0, fmt.Errorf("errWriter: write %d bytes at offset %d exceeds limit %d", len(p), w.written, w.limit)
	}
	w.written += len(p)
	return len(p), nil
}

// countWriter 只计数不落盘：先跑一遍校准 packageTo 的实际输出量，
// 阈值不写死字节数，避免耦合 flate 缓冲行为。
type countWriter struct{ n int }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}

// TestPackageFinalizeErrorPropagates 失败路径必须把错误上抛（历史 bug：
// 未命名返回值导致 close 链 flush 失败被吞、产出损坏 tgz 却报成功）：
//   - finalize：主体数据全部 Write 成功、错误只出现在收尾阶段
//     （tar/gzip Close 的 flush 输出），只能靠 packageTo 命名返回值在
//     defer 里上抛——"failed to finalize" 前缀即 walk 已完全成功的证明；
//   - midwalk：主体写入中途失败，错误沿 WalkDir/io.Copy 路径上抛；
//   - Package 层面：outDir 不存在 / 无写权限时文件创建失败。
func TestPackageFinalizeErrorPropagates(t *testing.T) {
	// 收尾阶段失败：小 chart 的压缩输出在 walk 期间几乎全部留在 flate
	// 缓冲里，close 时才落盘。阈值取 total-1：walk 输出 ≤ total-8
	//（gzip Close 至少写 8 字节 CRC/长度尾），必然全部成功；剩余容量
	// 小于收尾需求，失败必然落在 close 链。
	t.Run("finalize", func(t *testing.T) {
		dir := writeChart(t)
		c, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		var cw countWriter
		if err := packageTo(&cw, c, dir); err != nil {
			t.Fatalf("校准跑不应失败: %v", err)
		}
		if cw.n < 64 {
			t.Fatalf("校准输出异常小: %d", cw.n)
		}
		w := &errWriter{limit: cw.n - 1}
		err = packageTo(w, c, dir)
		if err == nil {
			t.Fatal("收尾写失败必须上抛非 nil 错误")
		}
		if !strings.Contains(err.Error(), "failed to finalize package") {
			t.Fatalf("主体写完后 close 链失败应报 finalize 错误: %v", err)
		}
		if !strings.Contains(err.Error(), "errWriter") {
			t.Fatalf("错误应源自底层 Writer: %v", err)
		}
	})

	// 主体中途失败：小 chart 在 walk 期间几乎不向底层写出（flate 缓冲），
	// 需要不可压缩大文件逼 gzip 持续落盘，错误才能在 io.Copy 路径浮出。
	// 阈值取总量一半：walk 输出约占总量的 99.9%，必在主体中途越限。
	t.Run("midwalk", func(t *testing.T) {
		dir := t.TempDir()
		for rel, content := range map[string]string{
			"chart.yaml":  "name: big\nversion: 1.0.0\n",
			"deploy.yaml": "- hosts: all\n  tasks: [{debug: {msg: x}}]\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// LCG 填充 1MiB 不可压缩数据：确定性生成，无随机依赖
		blob := make([]byte, 1<<20)
		var s uint32 = 0x12345678
		for i := range blob {
			s = s*1664525 + 1013904223
			blob[i] = byte(s >> 23)
		}
		if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "assets", "big.bin"), blob, 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		var cw countWriter
		if err := packageTo(&cw, c, dir); err != nil {
			t.Fatalf("校准跑不应失败: %v", err)
		}
		w := &errWriter{limit: cw.n / 2}
		if err := packageTo(w, c, dir); err == nil || !strings.Contains(err.Error(), "failed to package") {
			t.Fatalf("主体写入中途失败应经 WalkDir 路径上抛: %v", err)
		}
	})

	// Package 层面：产物文件创建失败
	t.Run("outDirMissing", func(t *testing.T) {
		dir := writeChart(t)
		if _, err := Package(dir, filepath.Join(t.TempDir(), "no", "such", "dir")); err == nil {
			t.Fatal("outDir 不存在必须报错")
		}
	})
	t.Run("outDirNoPermission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 不受目录写权限限制，无法模拟")
		}
		dir := writeChart(t)
		outDir := t.TempDir()
		if err := os.Chmod(outDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(outDir, 0o700) }) // 恢复权限，TempDir 清理才不会失败
		out, err := Package(dir, outDir)
		if err == nil {
			os.Remove(out)
			t.Fatal("无写权限的 outDir 必须报错")
		}
	})
}
