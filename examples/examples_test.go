// Package examples 是示例目录的回归测试：所有示例必须保持可运行。
//
// 覆盖三类示例：
//   - features/   playbook 特性矩阵（check 演练 + 关键路径真实执行）
//   - inventory-demo/  变量合并优先级与多文件合并
//   - chart-demo/ webstack/  应用包 lint + 三相位演练 + 真实部署闭环
//
// 执行环境要求：本机 POSIX sh（macOS/Linux 均可）；系统级模块（package/user/
// service/systemd_unit）在非 Linux 主机上由 when 守卫跳过，测试不依赖 root。
package examples_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/connection"
	_ "wdp/internal/connection/localconn" // 注册 local 连接工厂（演练主机）
	"wdp/internal/executor"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/playbook"
	"wdp/internal/render"
	"wdp/internal/report"
)

func mustAbs(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// loadPlaybookInventory 解析 features/local.yaml（4 台本机主机）。
func loadPlaybookInventory(t *testing.T) *inventory.Inventory {
	t.Helper()
	data, err := os.ReadFile(mustAbs(t, "features/local.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// runPlaybook 以真实模式执行 playbook（check=true 时为预演）。
func runPlaybook(t *testing.T, pbFile string, check bool) bool {
	t.Helper()
	plays, err := playbook.Load(pbFile)
	if err != nil {
		t.Fatalf("加载 %s 失败: %v", pbFile, err)
	}
	inv := loadPlaybookInventory(t)
	rep := report.NewConsole(io.Discard, false, -1)
	ex := executor.New(inv, connection.NewManager(), rep, executor.Options{
		Forks: 4, CheckMode: check, BaseDir: filepath.Dir(pbFile), Phase: "deploy",
	})
	return ex.Run(context.Background(), plays)
}

// TestFeaturesPlaybooks 特性矩阵全量 check 演练：任何引擎回归（解析/渲染/模块
// 预演）都会在这里暴露。rollback-demo 以 check 模式运行（门预演通过）。
func TestFeaturesPlaybooks(t *testing.T) {
	for _, f := range []string{
		"features/modules.yaml",
		"features/task-controls.yaml",
		"features/block-rescue.yaml",
		"features/serial-strategy.yaml",
		"features/rollback-demo.yaml",
		"features/dynamic-groups.yaml",
	} {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if runPlaybook(t, mustAbs(t, f), true) {
				t.Fatalf("check 演练 %s 不应失败", f)
			}
		})
	}
}

// TestModulesPlaybookRealRun modules.yaml 真实执行两轮：
// 第一轮建立产物，第二轮验证幂等收敛且不失败。
func TestModulesPlaybookRealRun(t *testing.T) {
	pbFile := mustAbs(t, "features/modules.yaml")
	t.Cleanup(func() {
		os.RemoveAll("/tmp/wdp-features")
		os.RemoveAll("/tmp/wdp-features-fetched")
	})
	if runPlaybook(t, pbFile, false) {
		t.Fatal("第一轮真实执行不应失败")
	}
	if runPlaybook(t, pbFile, false) {
		t.Fatal("幂等复跑不应失败")
	}
	if _, err := os.Stat("/tmp/wdp-features/l1/app.conf"); err != nil {
		t.Fatalf("app.conf 应已存在: %v", err)
	}
}

// TestRollbackDemoRealRun rollback-demo 真实执行：健康门必失败，
// 金丝雀批次的文件类变更被自动回滚（产物不残留）。
func TestRollbackDemoRealRun(t *testing.T) {
	pbFile := mustAbs(t, "features/rollback-demo.yaml")
	if !runPlaybook(t, pbFile, false) {
		t.Fatal("健康门失败的发布应判定失败")
	}
	if _, err := os.Stat("/tmp/wdp-rollback-demo-l1"); !os.IsNotExist(err) {
		t.Fatalf("回滚后产物不应残留（auto_rollback 未生效?）: %v", err)
	}
}

// TestInventoryDemoVars inventory-demo：变量优先级（host_vars > 组 vars > all.vars）
// 与 group_names 去重、children 组变量生效。
func TestInventoryDemoVars(t *testing.T) {
	inv, err := inventory.LoadMerge([]string{mustAbs(t, "inventory-demo/inventory.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host, key, want string
	}{
		{"web1", "nginx_port", "8081"}, // host_vars/web1.yaml 覆盖组变量
		{"web2", "nginx_port", "8080"}, // 组变量
		{"web1", "env", "dev"},         // all.vars
		{"web1", "company", "example-corp"},
		{"db1", "alert_channel", "#ops-prod"}, // children 组变量对成员生效
	}
	for _, c := range cases {
		h := inv.HostByName(c.host)
		if h == nil {
			t.Fatalf("主机 %s 不存在", c.host)
		}
		if got := fmt.Sprint(h.Vars[c.key]); got != c.want {
			t.Errorf("%s.%s = %v, 期望 %s", c.host, c.key, got, c.want)
		}
	}
	if got := inv.HostByName("db1").Vars["nginx_port"]; got != nil {
		t.Errorf("db1 不应继承 webservers 组变量 nginx_port，实际 %v", got)
	}
	// group_names 无重复（children 展开曾导致重复的回归点）
	gn, ok := inv.HostByName("web1").Vars["group_names"].([]string)
	if !ok {
		t.Fatalf("group_names 类型异常: %T", inv.HostByName("web1").Vars["group_names"])
	}
	seen := map[string]bool{}
	for _, g := range gn {
		if seen[g] {
			t.Errorf("group_names 出现重复: %v", gn)
		}
		seen[g] = true
	}
}

// TestInventoryDemoMerge 多文件合并：后者覆盖 all.vars、追加主机、深合并组变量。
func TestInventoryDemoMerge(t *testing.T) {
	inv, err := inventory.LoadMerge([]string{
		mustAbs(t, "inventory-demo/inventory.yaml"),
		mustAbs(t, "inventory-demo/inventory-prod.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(inv.HostByName("web2").Vars["env"]); got != "prod" {
		t.Errorf("第二文件应覆盖 all.vars: env=%v", got)
	}
	if got := fmt.Sprint(inv.HostByName("web2").Vars["nginx_port"]); got != "9090" {
		t.Errorf("第二文件应覆盖组变量: nginx_port=%v", got)
	}
	if got := fmt.Sprint(inv.HostByName("web2").Vars["tier"]); got != "web" {
		t.Errorf("原组变量应保留: tier=%v", got)
	}
	if got := fmt.Sprint(inv.HostByName("web2").Vars["tls"]); got != "true" {
		t.Errorf("第二文件新增键应合并: tls=%v", got)
	}
	if inv.HostByName("web3") == nil {
		t.Error("第二文件追加的主机 web3 应存在")
	}
	if got := fmt.Sprint(inv.HostByName("web1").Vars["nginx_port"]); got != "8081" {
		t.Errorf("host_vars 优先级应仍高于第二文件的组变量: %s", got)
	}
}

// loadChart 加载 + lint + required 校验（应用包质量门）。
func loadChart(t *testing.T, root string) *chart.Chart {
	t.Helper()
	c, err := chart.Load(root)
	if err != nil {
		t.Fatalf("chart.Load(%s) 失败: %v", root, err)
	}
	t.Cleanup(func() { c.Close() })
	values, err := c.BuildValues(nil, nil)
	if err != nil {
		t.Fatalf("BuildValues 失败: %v", err)
	}
	if err := c.ValidateRequired(values); err != nil {
		t.Fatalf("required 校验失败: %v", err)
	}
	for _, issue := range chart.Lint(c, values) {
		if issue.Level == chart.ERROR {
			t.Fatalf("lint 发现错误: %s", issue)
		}
	}
	return c
}

// runChartPhase 运行 chart 的指定相位（deploy/uninstall/status；check 控制预演），
// valuesFiles/sets 对应 -f 与 --set。返回是否存在失败。
func runChartPhase(t *testing.T, root, phase string, check bool, valuesFiles, sets []string) bool {
	t.Helper()
	c, err := chart.Load(root)
	if err != nil {
		t.Fatalf("chart.Load(%s) 失败: %v", root, err)
	}
	t.Cleanup(func() { c.Close() })
	values, err := c.BuildValues(valuesFiles, sets)
	if err != nil {
		t.Fatalf("BuildValues 失败: %v", err)
	}
	if err := c.ValidateRequired(values); err != nil {
		t.Fatalf("required 校验失败: %v", err)
	}
	eng, err := render.NewEngine(c.CollectHelpers())
	if err != nil {
		t.Fatal(err)
	}
	var plays []*model.Play
	switch phase {
	case "uninstall":
		plays = c.Uninstall
	case "status":
		plays = c.Status
	default:
		plays = c.Deploy
	}
	inv, err := inventory.LoadMerge([]string{mustAbs(t, filepath.Join(root, "inventory.yaml"))})
	if err != nil {
		t.Fatalf("加载 %s/inventory.yaml 失败: %v", root, err)
	}
	rep := report.NewConsole(io.Discard, false, -1)
	ex := executor.New(inv, connection.NewManager(), rep, executor.Options{
		Forks: 4, CheckMode: check, Chart: c, Values: values, Engine: eng,
		BaseDir: c.Dir, Phase: phase, WdpVersion: "test",
	})
	return ex.Run(context.Background(), plays)
}

// TestChartDemoCheck chart-demo：lint 质量门 + 三相位 check 演练。
func TestChartDemoCheck(t *testing.T) {
	root := mustAbs(t, "chart-demo")
	loadChart(t, root)
	for _, phase := range []string{"deploy", "status", "uninstall"} {
		if runChartPhase(t, root, phase, true, nil, nil) {
			t.Errorf("chart-demo %s 相位 check 演练不应失败", phase)
		}
	}
}

// TestWebstackLifecycle webstack 完整生命周期：
// lint → 三相位 check → 真实部署（隔离 workdir）→ 产物断言 → 幂等复跑 → status → 卸载。
func TestWebstackLifecycle(t *testing.T) {
	root := mustAbs(t, "webstack")
	loadChart(t, root)

	// 三相位零风险预演
	for _, phase := range []string{"deploy", "status", "uninstall"} {
		if runChartPhase(t, root, phase, true, nil, nil) {
			t.Errorf("webstack %s 相位 check 演练不应失败", phase)
		}
	}

	// 真实部署：workdir/conf_dir 隔离到临时目录（--set 第 3 层覆盖）
	work := t.TempDir()
	sets := []string{
		"global.workdir=" + filepath.Join(work, "orders"),
		"nginx.conf_dir=" + filepath.Join(work, "nginx"),
	}
	t.Cleanup(func() { os.RemoveAll("/tmp/wdp-webstack-marker") })
	if runChartPhase(t, root, "deploy", false, nil, sets) {
		t.Fatal("webstack 真实部署不应失败")
	}
	// 产物断言：版本目录 + current 软链 + 共享配置 + 反代配置
	ordersDir := filepath.Join(work, "orders")
	link, err := os.Readlink(filepath.Join(ordersDir, "current"))
	if err != nil {
		t.Fatalf("current 软链应存在: %v", err)
	}
	if want := filepath.Join(ordersDir, "releases", "1.4.2"); link != want {
		t.Errorf("current 指向 %s, 期望 %s", link, want)
	}
	if _, err := os.Stat(filepath.Join(ordersDir, "shared", "app.conf")); err != nil {
		t.Fatalf("shared/app.conf 应存在: %v", err)
	}
	nginxCnf, err := os.ReadFile(filepath.Join(work, "nginx", "webstack.conf"))
	if err != nil {
		t.Fatalf("nginx webstack.conf 应存在: %v", err)
	}
	if !strings.Contains(string(nginxCnf), "upstream dev-backend") {
		t.Errorf("nginx.conf 应含自动发现的 upstream:\n%s", nginxCnf)
	}
	if !strings.Contains(string(nginxCnf), "server app1:8080") {
		t.Errorf("nginx.conf 应含 appservers 成员地址:\n%s", nginxCnf)
	}

	// 幂等复跑 + 状态相位
	if runChartPhase(t, root, "deploy", false, nil, sets) {
		t.Fatal("webstack 幂等复跑不应失败")
	}
	if runChartPhase(t, root, "status", false, nil, sets) {
		t.Fatal("webstack status 相位不应失败")
	}

	// 卸载：目录与 marker 清理
	if runChartPhase(t, root, "uninstall", false, nil, sets) {
		t.Fatal("webstack 卸载不应失败")
	}
	if _, err := os.Stat(ordersDir); !os.IsNotExist(err) {
		t.Errorf("卸载后应用目录应删除: %v", err)
	}
	if _, err := os.Stat("/tmp/wdp-webstack-marker/webstack/release.json"); !os.IsNotExist(err) {
		t.Error("卸载后 release marker 应清除")
	}
}

// TestWebstackValuesLayering 三层 values 深合并：envs/prod.yaml 覆盖标量与列表、
// 递归合并嵌套 map（cache.driver 换 redis 而 ttl_secs 保留）。
func TestWebstackValuesLayering(t *testing.T) {
	c, err := chart.Load(mustAbs(t, "webstack"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	values, err := c.BuildValues([]string{mustAbs(t, "webstack/envs/prod.yaml")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := values["app"].(map[string]any)
	if got := fmt.Sprint(app["replicas"]); got != "4" {
		t.Errorf("prod replicas = %v, 期望 4", app["replicas"])
	}
	features := app["features"].(map[string]any)
	cache := features["cache"].(map[string]any)
	if got := fmt.Sprint(cache["driver"]); got != "redis" {
		t.Errorf("prod cache.driver = %v, 期望 redis（覆盖）", cache["driver"])
	}
	if got := fmt.Sprint(cache["ttl_secs"]); got != "300" {
		t.Errorf("prod cache.ttl_secs = %v, 期望 300（深合并保留默认值）", cache["ttl_secs"])
	}
	if got := fmt.Sprint(features["tracing"]); got != "true" {
		t.Errorf("prod tracing = %v, 期望 true", features["tracing"])
	}
	if got := fmt.Sprint(features["metrics"]); got != "true" {
		t.Errorf("prod metrics = %v, 期望 true（默认值保留）", features["metrics"])
	}
	nginx := values["nginx"].(map[string]any)
	if got := fmt.Sprint(nginx["conf_dir"]); got != "/etc/nginx/conf.d" {
		t.Errorf("prod nginx.conf_dir = %v", nginx["conf_dir"])
	}
	// --set 第 3 层覆盖
	values2, err := c.BuildValues([]string{mustAbs(t, "webstack/envs/prod.yaml")}, []string{"app.replicas=8"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(values2["app"].(map[string]any)["replicas"]); got != "8" {
		t.Errorf("--set app.replicas=8 应覆盖 -f: %v", got)
	}
}
