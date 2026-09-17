package chart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePhaseChart 写出含自定义相位（update/stop）与保留名文件（inventory.yaml、
// 带点号的杂项 yaml）的 chart。
func writePhaseChart(t *testing.T, extraChartYAML string) string {
	t.Helper()
	dir := t.TempDir()
	must := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("chart.yaml", "name: myapp\nversion: 1.0.0\n"+extraChartYAML)
	must("values.yaml", "app: {name: demo}\n")
	must("deploy.yaml", "- hosts: all\n  tasks:\n    - copy: {content: x, dest: /etc/app.conf}\n")
	must("uninstall.yaml", "- hosts: all\n  tasks:\n    - file: {path: /etc/app.conf, state: absent}\n")
	must("update.yaml", "- hosts: all\n  tasks:\n    - shell: 'migrate.sh'\n")
	must("stop.yaml", "- hosts: all\n  tasks:\n    - service: {name: app, state: stopped}\n")
	// 保留名/非相位名：不作为相位加载，也不应报错
	must("inventory.yaml", "all:\n  hosts:\n    h1: {ansible_host: 10.0.0.1}\n")
	must("notes.extra.yaml", "not: a playbook\n")
	return dir
}

func TestPhaseDiscovery(t *testing.T) {
	c, err := Load(writePhaseChart(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"uninstall", "update", "stop"} {
		plays, err := c.PhasePlays(phase)
		if err != nil || len(plays) != 1 {
			t.Fatalf("相位 %s 应可用: %v", phase, err)
		}
	}
	// 保留名与杂项 yaml 不当相位
	for _, notPhase := range []string{"inventory", "notes.extra", "values", "chart"} {
		if _, err := c.PhasePlays(notPhase); err == nil {
			t.Fatalf("%s 不应被当作相位", notPhase)
		}
	}
	// PhaseNames：deploy 在前，其余字典序
	if got := strings.Join(c.PhaseNames(), ","); got != "deploy,stop,uninstall,update" {
		t.Fatalf("PhaseNames: %s", got)
	}
}

func TestPhasePlaysUnknownListsAvailable(t *testing.T) {
	c, err := Load(writePhaseChart(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.PhasePlays("nonsense")
	if err == nil || !strings.Contains(err.Error(), "deploy,") || !strings.Contains(err.Error(), "update") {
		t.Fatalf("未知相位应列出可用相位: %v", err)
	}
	// 空串按 deploy
	if plays, err := c.PhasePlays(""); err != nil || len(plays) != 1 {
		t.Fatalf("空串应按 deploy: %v", err)
	}
}

func TestPhaseSpecDefaultsAndDeclaration(t *testing.T) {
	// 内置缺省
	if s := DefaultPhaseSpec("deploy"); !s.Release || !s.Records() {
		t.Fatalf("deploy 缺省: %+v", s)
	}
	if s := DefaultPhaseSpec("uninstall"); s.Release || !s.Records() || !s.ClearsMarker {
		t.Fatalf("uninstall 缺省: %+v", s)
	}
	if s := DefaultPhaseSpec("status"); s.Records() || s.Release || s.ClearsMarker {
		t.Fatalf("status 缺省应为零值: %+v", s)
	}
	if s := DefaultPhaseSpec(""); !s.Release {
		t.Fatal("空串应按 deploy")
	}
	// 未声明的自定义相位：零值
	c, err := Load(writePhaseChart(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.PhaseSpecFor("stop"); s.Records() || s.Release || s.ClearsMarker {
		t.Fatalf("未声明自定义相位应为零值: %+v", s)
	}

	// chart.yaml 声明：release 语义 + uninstall 内置不可关闭
	declared, err := Load(writePhaseChart(t, "phases:\n  update: {release: true}\n  stop: {record: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s := declared.PhaseSpecFor("update"); !s.Release || !s.Records() || s.ClearsMarker {
		t.Fatalf("update 声明 release: %+v", s)
	}
	if s := declared.PhaseSpecFor("stop"); s.Release || !s.Records() {
		t.Fatalf("stop 声明 record: %+v", s)
	}
	if s := declared.PhaseSpecFor("uninstall"); !s.Records() || !s.ClearsMarker {
		t.Fatalf("uninstall 内置属性不受声明影响: %+v", s)
	}
}

func TestPhaseNameValidation(t *testing.T) {
	// phases 键名非法（路径分隔符/点号等）在加载入口拒绝
	for _, bad := range []string{"a/b: {}", "a.b: {}", "1abc: {}", "'': {}"} {
		dir := writePhaseChart(t, "phases:\n  "+bad+"\n")
		if _, err := Load(dir); err == nil {
			t.Fatalf("非法 phases 键 %q 应报错", bad)
		}
	}
	// values_from 只接受 chart / marker
	dir := writePhaseChart(t, "phases:\n  update: {values_from: env}\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("非法 values_from 应报错")
	}
}

// TestPhaseSpecValuesFrom values 来源的缺省推导与显式覆盖：
// 只有清除 marker 的相位（卸载类）读 marker，其余相位读 chart values——
// 否则纯准备/只读相位会被"必须先部署过"卡死。
func TestPhaseSpecValuesFrom(t *testing.T) {
	def := map[string]ValuesFrom{
		"":          ValuesFromChart, // 空串 = deploy
		"deploy":    ValuesFromChart,
		"status":    ValuesFromChart, // 只读相位不再依赖 marker
		"download":  ValuesFromChart, // 自定义零值相位
		"uninstall": ValuesFromMarker,
	}
	for phase, want := range def {
		if got := DefaultPhaseSpec(phase).EffectiveValuesFrom(); got != want {
			t.Errorf("DefaultPhaseSpec(%q).EffectiveValuesFrom() = %q, want %q", phase, got, want)
		}
	}
	// 声明覆盖：purge 清除 marker 但显式改读 chart values
	spec := MergePhaseSpec(DefaultPhaseSpec("purge"), PhaseSpec{ClearsMarker: true, ValuesFrom: ValuesFromChart})
	if spec.EffectiveValuesFrom() != ValuesFromChart {
		t.Fatalf("显式 values_from 应覆盖推导: %+v", spec)
	}
	// 声明覆盖：status 显式改读 marker（需要已部署入参的场景）
	spec = MergePhaseSpec(DefaultPhaseSpec("status"), PhaseSpec{ValuesFrom: ValuesFromMarker})
	if spec.EffectiveValuesFrom() != ValuesFromMarker {
		t.Fatalf("status 显式 values_from: marker: %+v", spec)
	}
	// 未声明 values_from 时保持推导
	spec = MergePhaseSpec(DefaultPhaseSpec("uninstall"), PhaseSpec{Record: true})
	if spec.EffectiveValuesFrom() != ValuesFromMarker {
		t.Fatalf("uninstall 推导应保持 marker: %+v", spec)
	}
}

func TestHookNameFor(t *testing.T) {
	for phase, want := range map[string]string{"": "install", "deploy": "install", "uninstall": "uninstall", "update": "update"} {
		if got := HookNameFor(phase); got != want {
			t.Fatalf("HookNameFor(%q) = %q, want %q", phase, got, want)
		}
	}
}

// TestNormalizeHook pre_deploy/post_deploy 是 pre_install/post_install 的
// 等价别名（"词干即相位名"的直觉写法），其余名字原样返回。
func TestNormalizeHook(t *testing.T) {
	for in, want := range map[string]string{
		"pre_deploy":    "pre_install",
		"post_deploy":   "post_install",
		"pre_install":   "pre_install",
		"post_install":  "post_install",
		"pre_uninstall": "pre_uninstall",
		"pre_update":    "pre_update",
		"":              "",
	} {
		if got := NormalizeHook(in); got != want {
			t.Errorf("NormalizeHook(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLintAllPhases(t *testing.T) {
	dir := t.TempDir()
	must := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("chart.yaml", "name: myapp\nversion: 1.0.0\nphases:\n  bakcup: {record: true}\n") // 拼写：无 bakcup.yaml
	must("values.yaml", "app: {name: demo}\n")
	must("deploy.yaml", `
- hosts: all
  tasks:
    - shell: 'echo hi'
      hook: pre_stopp   # 拼写错误：没有 stopp 相位
`)
	must("uninstall.yaml", `
- hosts: all
  tasks:
    - nosuchmodule: {a: 1}   # 相位文件的未知模块：此前 lint 不查
`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	issues := Lint(c, map[string]any{"app": map[string]any{"name": "demo"}})
	var unknownModule, hookTypo, phaseTypo bool
	for _, i := range issues {
		if i.Level == ERROR && strings.Contains(i.Msg, "unknown module") && i.Path == "uninstall.yaml" {
			unknownModule = true
		}
		if i.Level == WARN && strings.Contains(i.Msg, "hook \"pre_stopp\"") {
			hookTypo = true
		}
		if i.Level == WARN && strings.Contains(i.Msg, "phases.bakcup") {
			phaseTypo = true
		}
	}
	if !unknownModule {
		t.Fatalf("应发现 uninstall.yaml 的未知模块: %v", issues)
	}
	if !hookTypo {
		t.Fatalf("应发现 hook 拼写告警: %v", issues)
	}
	if !phaseTypo {
		t.Fatalf("应发现 phases 声明无对应文件: %v", issues)
	}
}

func TestEntryPlay(t *testing.T) {
	c, err := Load(writePhaseChart(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	// 缺省与显式 deploy：deploy.yaml 的 play
	for _, phase := range []string{"", "deploy"} {
		p, err := c.EntryPlay(phase)
		if err != nil || p != c.Deploy[0] {
			t.Fatalf("EntryPlay(%q) 应返回 deploy play: %v", phase, err)
		}
	}
	// 自定义相位入口
	p, err := c.EntryPlay("update")
	if err != nil || len(p.Tasks) != 1 || p.Tasks[0].Module != "shell" {
		t.Fatalf("EntryPlay(update) 异常: %v %+v", err, p)
	}
	// 未知相位：报错并列出可用相位
	if _, err := c.EntryPlay("nonsense"); err == nil || !strings.Contains(err.Error(), "update") {
		t.Fatalf("未知入口相位应列出可用相位: %v", err)
	}
}

func TestEntryPlayMultiPlayRejected(t *testing.T) {
	// 多 play 相位无法内联展开进父 play 任务序列
	dir := writePhaseChart(t, "")
	if err := os.WriteFile(filepath.Join(dir, "update.yaml"), []byte(
		"- hosts: all\n  tasks:\n    - shell: 'echo 1'\n- hosts: all\n  tasks:\n    - shell: 'echo 2'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EntryPlay("update"); err == nil || !strings.Contains(err.Error(), "only one is supported") {
		t.Fatalf("多 play 入口相位应报错: %v", err)
	}
}

func TestAllPlays(t *testing.T) {
	c, err := Load(writePhaseChart(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	// deploy 在前，其余相位全覆盖（handler 合并依赖此完整性）
	plays := c.AllPlays()
	if len(plays) != 4 { // deploy + uninstall + update + stop
		t.Fatalf("AllPlays 应覆盖全部相位: %d", len(plays))
	}
	if plays[0] != c.Deploy[0] {
		t.Fatal("AllPlays 首位应为 deploy play")
	}
}

// 相位 play 经 chart 层加载后的结构完整性（include 展开等由 playbook 层覆盖）。
func TestPhasePlaysStructure(t *testing.T) {
	c, err := Load(writePhaseChart(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	plays := c.Phases["update"]
	if len(plays) != 1 || plays[0].Tasks[0].Module != "shell" {
		t.Fatalf("update.yaml 解析结果异常: %+v", plays)
	}
}
