package plan

// §6.4 P1 验证：plan 的确定性（逐字节相同）、离线编译（不连接主机）、
// 文件快照物化往返、连接元数据脱敏。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/inventory"
	"wdp/internal/model"
)

// writeCompileChart 写出多模块 chart（含 hook / block / chart 引用 / 模板）。
func writeCompileChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", "name: app\nversion: 2.1.0\nrequired: [app.port]\n")
	write("values.yaml", "app: {name: demo, port: 8080}\n")
	write("_helpers.tpl", "{{- define \"app.fullname\" -}}{{ .app.name }}-wdp{{- end }}\n")
	write("deploy.yaml", `
- hosts: webservers
  become: true
  tasks:
    - name: pre hook
      shell: 'echo pre'
      hook: pre_install
    - name: config
      template: {src: templates/app.conf.tpl, dest: /etc/app.conf}
      notify: [reload]
    - name: nested
      block:
        - shell: 'echo a'
        - shell: 'echo b'
      rescue:
        - shell: 'echo rescue'
    - chart: jdk
  handlers:
    - name: reload
      shell: 'systemctl reload app'
`)
	write("templates/app.conf.tpl", "port={{ .app.port }}\nhost={{ .inventory_hostname }}\n")
	write("charts/jdk/chart.yaml", "name: jdk\nversion: 11.0.0\n")
	write("charts/jdk/values.yaml", "version: \"11\"\n")
	write("charts/jdk/deploy.yaml", "- hosts: all\n  tasks:\n    - shell: 'java -version'\n")
	return dir
}

const compileInv = `
webservers:
  hosts:
    w1: {ansible_host: 10.0.0.1}
    w2: {ansible_host: 10.0.0.2}
dbs:
  hosts:
    db1: {ansible_host: 10.0.1.1}
`

// compileOnce 编译一次（webservers 相位 play）。
func compileOnce(t *testing.T, sets []string) *Plan {
	t.Helper()
	inv, err := inventory.Parse([]byte(compileInv))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(writeCompileChart(t), inv, nil, sets, CompileOptions{WdpVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCompileDeterministic 同一 chart + 同一 values 两次编译逐字节相同
// （PlanID 内容寻址与 plan diff 成立的前提）。
func TestCompileDeterministic(t *testing.T) {
	a := compileOnce(t, []string{"app.port=9090"})
	b := compileOnce(t, []string{"app.port=9090"})
	if a.PlanID != b.PlanID {
		t.Fatalf("PlanID 不确定: %s != %s", a.PlanID, b.PlanID)
	}
	da, _ := json.Marshal(a)
	db, _ := json.Marshal(b)
	if string(da) != string(db) {
		t.Fatal("两次编译产物应逐字节相同")
	}
	// 不同 values → 不同 PlanID
	c := compileOnce(t, []string{"app.port=9091"})
	if c.PlanID == a.PlanID {
		t.Fatal("不同 values 应产生不同 PlanID")
	}
}

// TestCompileOffline 离线编译：inventory 指向不可达地址也不连接任何主机
// （部署相位 values 完全来自控制端计算）。
func TestCompileOffline(t *testing.T) {
	invSrc := `
webservers:
  hosts:
    dead1: {ansible_host: 203.0.113.1, conn: ssh}
`
	inv, err := inventory.Parse([]byte(invSrc))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(writeCompileChart(t), inv, nil, nil, CompileOptions{WdpVersion: "test"})
	if err != nil {
		t.Fatalf("离线编译不应失败: %v", err)
	}
	if len(p.Hosts) != 1 || p.Hosts[0].Host != "dead1" {
		t.Fatalf("host plans: %+v", p.Hosts)
	}
}

// TestCompileFrozenVars 冻结变量域含跨主机信息：groups/hosts/hostvars/
// play_hosts 固化为字面值，hostvars 含其他主机的 inventory 变量。
func TestCompileFrozenVars(t *testing.T) {
	p := compileOnce(t, nil)
	hp := p.Hosts[0]
	if hp.Vars["inventory_hostname"] != hp.Host {
		t.Fatalf("inventory_hostname: %v", hp.Vars["inventory_hostname"])
	}
	groups, ok := hp.Vars["groups"].(map[string][]string)
	if !ok {
		t.Fatalf("groups 快照类型异常: %T", hp.Vars["groups"])
	}
	if _, has := groups["webservers"]; !has {
		t.Fatalf("groups 快照缺 webservers: %v", groups)
	}
	hostvars, ok := hp.Vars["hostvars"].(map[string]map[string]any)
	if !ok {
		t.Fatalf("hostvars 快照类型异常: %T", hp.Vars["hostvars"])
	}
	if len(hostvars) != 3 {
		t.Fatalf("hostvars 应含全部 3 台主机: %v", hostvars)
	}
	playHosts := hp.Vars["play_hosts"].([]string)
	if len(playHosts) != 2 || playHosts[0] != "w1" {
		t.Fatalf("play_hosts: %v", playHosts)
	}
	// playbook_dir 不冻结（执行侧 BaseDir 是物化目录）
	if _, ok := hp.Vars["playbook_dir"]; ok {
		t.Fatal("playbook_dir 不应冻结")
	}
}

// TestCompileHookSplitAndIdx hook 拆分与主机内稳定序号。
func TestCompileHookSplitAndIdx(t *testing.T) {
	p := compileOnce(t, nil)
	hp := p.Hosts[0]
	if len(hp.Pre) != 1 || hp.Pre[0].Label != "pre hook" {
		t.Fatalf("pre hook 拆分: %+v", hp.Pre)
	}
	if len(hp.Tasks) == 0 || hp.Tasks[0].Label != "config" {
		t.Fatalf("主任务列表: %+v", hp.Tasks)
	}
	if len(hp.Handlers) != 1 || hp.Handlers[0].Label != "reload" {
		t.Fatalf("handlers: %+v", hp.Handlers)
	}
	// idx 从 1 起、连续且 pre 在前
	if hp.Pre[0].Idx != 1 || hp.Tasks[0].Idx != 2 {
		t.Fatalf("idx 编号: pre=%d tasks=%d", hp.Pre[0].Idx, hp.Tasks[0].Idx)
	}
	// 两台主机同一 play：任务一致，idx 独立编号（都从 1 起）
	if p.Hosts[1].Tasks[0].Idx != 2 {
		t.Fatalf("第二台主机 idx 应独立: %+v", p.Hosts[1].Tasks)
	}
	// block 嵌套保留
	var found bool
	for _, rt := range hp.Tasks {
		if rt.Label == "nested" && len(rt.Block) == 2 && len(rt.Rescue) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("block 树应保留: %+v", hp.Tasks)
	}
	// chart 引用保留（执行侧展开，语义单一实现）
	var hasChartRef bool
	for _, rt := range hp.Tasks {
		if rt.ChartRef == "jdk" {
			hasChartRef = true
		}
	}
	if !hasChartRef {
		t.Fatal("chart 引用应保留在计划中")
	}
}

// TestPlanWriteLoadRoundTrip 落盘/加载往返 + PlanID 完整性校验。
func TestPlanWriteLoadRoundTrip(t *testing.T) {
	p := compileOnce(t, nil)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := p.Write(path); err != nil {
		t.Fatal(err)
	}
	// 落盘权限 0600（values 可能含敏感配置）
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("plan 权限应 0600: %v %v", fi, err)
	}
	q, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if q.PlanID != p.PlanID || q.Chart != "app" {
		t.Fatalf("roundtrip: %s %s", q.PlanID, q.Chart)
	}
	// 篡改后 PlanID 校验失败
	data, _ := os.ReadFile(path)
	tampered := strings.Replace(string(data), `"port":8080`, `"port":9999`, 1)
	if tampered == string(data) { // 数值格式可能不同，未命中则跳过本断言
		t.Skip("port 字面量未命中")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("篡改的 plan 应被拒绝: %v", err)
	}
}

// TestMaterializeRoundTrip 文件快照物化往返：内容逐字节还原。
func TestMaterializeRoundTrip(t *testing.T) {
	dir := writeCompileChart(t)
	inv, _ := inventory.Parse([]byte(compileInv))
	p, err := Compile(dir, inv, nil, nil, CompileOptions{WdpVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := p.Materialize(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(filepath.Join(dir, "templates/app.conf.tpl"))
	got, err := os.ReadFile(filepath.Join(root, "templates/app.conf.tpl"))
	if err != nil || string(orig) != string(got) {
		t.Fatalf("物化内容不一致: %v", err)
	}
}

// TestHostConnSecretStripping 连接元数据脱敏：明文密钥剔除，env 引用保留。
func TestHostConnSecretStripping(t *testing.T) {
	h := compileHostWithSecrets()
	c := hostConnOf(h)
	if c.PasswordEnvRef != "" || c.BecomePasswordRef != "" {
		t.Fatalf("明文密钥不应进计划: %+v", c)
	}
	h2 := compileHostWithEnvRefs()
	c2 := hostConnOf(h2)
	if c2.PasswordEnvRef != "env:SSH_PW" || c2.BecomePasswordRef != "env:BECOME_PW" {
		t.Fatalf("env 引用应保留: %+v", c2)
	}
	// 还原后 Secret 从环境解析
	os.Setenv("SSH_PW", "s3cret")
	defer os.Unsetenv("SSH_PW")
	rt := c2.Host("h1")
	if rt.Password != "env:SSH_PW" || model.Secret(rt.Password, rt.PasswordEnv) != "s3cret" {
		t.Fatalf("env 引用还原: %q", rt.Password)
	}
}

// TestMarkerPhaseNeedsHostValues 非部署相位要求调用方传入 marker 还原值
// （离线编译器不自行连接主机）。
func TestMarkerPhaseNeedsHostValues(t *testing.T) {
	dir := writeCompileChart(t)
	un := filepath.Join(dir, "uninstall.yaml")
	if err := os.WriteFile(un, []byte("- hosts: all\n  tasks:\n    - shell: 'rm -rf /x'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inv, _ := inventory.Parse([]byte(compileInv))
	_, err := Compile(dir, inv, nil, nil, CompileOptions{Phase: "uninstall"})
	if err == nil || !strings.Contains(err.Error(), "markers") {
		t.Fatalf("非部署相位应要求 HostValues: %v", err)
	}
	// 传入后成功
	p, err := Compile(dir, inv, nil, nil, CompileOptions{Phase: "uninstall",
		HostValues: map[string]map[string]any{"w1": {"a": 1}, "w2": {"a": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Hosts[0].Values["a"] != 1 {
		t.Fatalf("HostPlan.Values 应取 HostValues: %+v", p.Hosts[0].Values)
	}
}

// TestCompilePayloads 制品采集：packages/ 下任何文件（与体积无关）与超过
// 单文件阈值的大文件都记入 Payloads 而非 plan 本体；快照总量超限即报错。
// golden 测试已把 Payloads 归一化剔除（本地制品缓存是否存在不影响 golden），
// 本测试是 payload 语义的唯一覆盖点。
func TestCompilePayloads(t *testing.T) {
	oldFile, oldTotal := maxFileBytes, maxTotalBytes
	defer func() { maxFileBytes, maxTotalBytes = oldFile, oldTotal }()
	maxFileBytes = 1 << 10  // 1 KiB：小文件即可覆盖"超限转 payload"
	maxTotalBytes = 2 << 10 // 2 KiB：叠加少量文件即可触发总量上限

	dir := writeCompileChart(t)
	writeBin := func(rel string, size int, fill byte) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte{fill}, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// packages/ 下即使只有 16 字节也进 payload
	writeBin("packages/small.bin", 16, 'a')
	// 超单文件阈值的大文件同样进 payload
	writeBin("big.bin", 1100, 'b')

	inv, err := inventory.Parse([]byte(compileInv))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(dir, inv, nil, nil, CompileOptions{WdpVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]PayloadRef{}
	for _, ref := range p.Payloads {
		byPath[ref.Path] = ref
	}
	for _, want := range []string{"packages/small.bin", "big.bin"} {
		ref, ok := byPath[want]
		if !ok {
			t.Fatalf("%s 应记入 Payloads: %+v", want, p.Payloads)
		}
		data, _ := os.ReadFile(filepath.Join(dir, want))
		sum := sha256.Sum256(data)
		if ref.Size != int64(len(data)) || ref.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s 的 size/sha256 不符: %+v", want, ref)
		}
		if _, embedded := p.Files[want]; embedded {
			t.Fatalf("%s 不应嵌入 plan 本体", want)
		}
	}
	if _, ok := p.Files["deploy.yaml"]; !ok {
		t.Fatal("小文件应嵌入 plan 本体")
	}

	// 总量超限：两个 800 字节文件叠加后越过 2 KiB 上限
	writeBin("bloat1.bin", 800, 'c')
	writeBin("bloat2.bin", 800, 'd')
	if _, err := Compile(dir, inv, nil, nil, CompileOptions{WdpVersion: "test"}); err == nil ||
		!strings.Contains(err.Error(), "plan total") {
		t.Fatalf("总量超限应报错: %v", err)
	}
}

// compileHostWithSecrets 构造带明文密钥的主机。
func compileHostWithSecrets() *model.Host {
	h := &model.Host{Name: "h1"}
	h.Password = "literal-password"
	h.BecomePassword = "literal-become"
	return h
}

func compileHostWithEnvRefs() *model.Host {
	h := &model.Host{Name: "h1"}
	h.Password = "env:SSH_PW"
	h.BecomePassword = "env:BECOME_PW"
	return h
}
