package module

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/model"
)

// statTestEnv 模拟远端文件系统供 stat 探测：类型/权限/大小/属主/链接/校验和。
type statTestEnv struct {
	kinds  map[string]string // 路径 → file/directory/link/missing
	modes  map[string]string // 路径 → 八进制权限
	sizes  map[string]int64
	owners map[string]string // 路径 → "user group"
	links  map[string]string // readlink -f 结果
	sums   map[string]string // sha256
}

// newStatTestRC 构造带 stat 模拟的执行上下文。
// Fake 需区分 remoteSize / remoteMode / remoteOwnerGroup 三类 stat 脚本，与 newTestRC 的通用模拟不同。
func newStatTestRC(t *testing.T) (*RunContext, *connection.Fake, *statTestEnv) {
	t.Helper()
	fake := connection.NewFake(&model.Host{Name: "test"})
	env := &statTestEnv{
		kinds:  map[string]string{},
		modes:  map[string]string{},
		sizes:  map[string]int64{},
		owners: map[string]string{},
		links:  map[string]string{},
		sums:   map[string]string{},
	}
	fake.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, "sha256sum"):
			sum, ok := env.sums[extractQuoted(s, "p=")]
			if !ok {
				return connection.ExecResult{Code: 3}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: sum + "  file\n"}, nil
		case strings.Contains(s, "readlink -f"):
			target, ok := env.links[extractQuoted(s, "readlink -f -- ")]
			if !ok {
				return connection.ExecResult{Code: 1}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: target + "\n"}, nil
		case strings.Contains(s, "stat -c %s"), strings.Contains(s, "stat -f %z"):
			size, ok := env.sizes[extractQuoted(s, "p=")]
			if !ok {
				return connection.ExecResult{Code: 3}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: strconv.FormatInt(size, 10)}, nil
		case strings.Contains(s, "stat -c '%U %G'"):
			og, ok := env.owners[extractQuoted(s, "p=")]
			if !ok {
				return connection.ExecResult{Code: 3}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: og + "\n"}, nil
		case strings.Contains(s, "stat -c %a"), strings.Contains(s, "stat -f %Lp"):
			mode, ok := env.modes[extractQuoted(s, "p=")]
			if !ok {
				return connection.ExecResult{Code: 3}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: mode}, nil
		case strings.Contains(s, "[ -L"):
			kind := env.kinds[extractQuoted(s, "p=")]
			if kind == "" {
				kind = "missing"
			}
			return connection.ExecResult{Code: 0, Stdout: kind + "\n"}, nil
		default:
			return connection.ExecResult{}, nil
		}
	}
	rc := &RunContext{
		Ctx:     context.Background(),
		Conn:    fake,
		Host:    fake.Host,
		Vars:    map[string]any{},
		BaseDir: ".",
	}
	return rc, fake, env
}

// seedStatFile 填充一个普通文件的完整模拟数据。
func seedStatFile(env *statTestEnv, path, content, mode, owner, group string) {
	env.kinds[path] = "file"
	env.modes[path] = mode
	env.sizes[path] = int64(len(content))
	env.owners[path] = owner + " " + group
	env.sums[path] = sha256hex([]byte(content))
}

func statFacts(t *testing.T, r *Result) map[string]any {
	t.Helper()
	if r.Failed {
		t.Fatalf("stat 失败: %s", r.Msg)
	}
	if r.Changed {
		t.Fatal("stat 永不变更")
	}
	facts, ok := r.Facts["stat"].(map[string]any)
	if !ok {
		t.Fatalf("Facts 缺 stat: %+v", r.Facts)
	}
	return facts
}

func TestStatFile(t *testing.T) {
	rc, _, env := newStatTestRC(t)
	content := "hello wdp\n"
	seedStatFile(env, "/etc/motd", content, "644", "root", "root")
	mod := &StatModule{}

	r := mod.Run(rc, map[string]any{"path": "/etc/motd"}, "")
	f := statFacts(t, r)
	if f["exists"] != true || f["isfile"] != true || f["isdir"] != false || f["islink"] != false {
		t.Fatalf("类型判定: %+v", f)
	}
	if f["mode"] != "0644" {
		t.Fatalf("mode %v（应为 4 位八进制串）", f["mode"])
	}
	if f["size"] != int64(len(content)) {
		t.Fatalf("size %v", f["size"])
	}
	if f["owner"] != "root" || f["group"] != "root" {
		t.Fatalf("属主 %v/%v", f["owner"], f["group"])
	}
	if f["checksum"] != sha256hex([]byte(content)) {
		t.Fatalf("checksum %v", f["checksum"])
	}
	if f["path"] != "/etc/motd" {
		t.Fatalf("path %v", f["path"])
	}
}

func TestStatMissingAndDirectory(t *testing.T) {
	rc, _, env := newStatTestRC(t)
	env.kinds["/data"] = "directory"
	env.modes["/data"] = "755"
	env.sizes["/data"] = 4096
	env.owners["/data"] = "root root"
	mod := &StatModule{}

	r := mod.Run(rc, map[string]any{"path": "/gone"}, "")
	f := statFacts(t, r)
	if f["exists"] != false || f["isfile"] != false || f["isdir"] != false {
		t.Fatalf("缺失路径应全 false: %+v", f)
	}
	if f["mode"] != "" || f["size"] != int64(0) || f["checksum"] != "" {
		t.Fatalf("缺失路径属性应为零值: %+v", f)
	}

	r = mod.Run(rc, map[string]any{"path": "/data"}, "")
	f = statFacts(t, r)
	if f["isdir"] != true || f["mode"] != "0755" || f["size"] != int64(4096) {
		t.Fatalf("目录 facts: %+v", f)
	}
	// 目录不采集校验和
	if f["checksum"] != "" {
		t.Fatalf("目录不应有 checksum: %+v", f)
	}
}

func TestStatNoChecksumAndLargeFile(t *testing.T) {
	rc, _, env := newStatTestRC(t)
	seedStatFile(env, "/opt/big.bin", "x", "644", "app", "app")
	env.sizes["/opt/big.bin"] = 2 << 20 // 超过 1MB 上限
	mod := &StatModule{}

	// get_checksum=false 显式关闭
	r := mod.Run(rc, map[string]any{"path": "/opt/big.bin", "get_checksum": false}, "")
	if f := statFacts(t, r); f["checksum"] != "" {
		t.Fatalf("get_checksum=false: %+v", f)
	}
	// 超 1MB 自动跳过
	r = mod.Run(rc, map[string]any{"path": "/opt/big.bin"}, "")
	if f := statFacts(t, r); f["checksum"] != "" {
		t.Fatalf("大文件不应计算 checksum: %+v", f)
	}
}

func TestStatFollowSymlink(t *testing.T) {
	rc, _, env := newStatTestRC(t)
	seedStatFile(env, "/opt/app-v2", "binary", "755", "app", "app")
	env.kinds["/opt/current"] = "link"
	env.links["/opt/current"] = "/opt/app-v2"
	mod := &StatModule{}

	// 不 follow：仅报告链接本身
	r := mod.Run(rc, map[string]any{"path": "/opt/current"}, "")
	f := statFacts(t, r)
	if f["islink"] != true || f["isfile"] != false {
		t.Fatalf("非 follow 链接判定: %+v", f)
	}

	// follow：解析目标后统计，islink 仍如实
	r = mod.Run(rc, map[string]any{"path": "/opt/current", "follow": true}, "")
	f = statFacts(t, r)
	if f["islink"] != true || f["isfile"] != true {
		t.Fatalf("follow 类型判定: %+v", f)
	}
	if f["mode"] != "0755" || f["owner"] != "app" || f["size"] != int64(6) {
		t.Fatalf("follow 应统计目标属性: %+v", f)
	}
	if f["checksum"] != sha256hex([]byte("binary")) {
		t.Fatalf("follow checksum: %+v", f)
	}
	if f["path"] != "/opt/current" {
		t.Fatalf("path 应保持请求路径: %+v", f)
	}
}

func TestStatCheckModeIdentical(t *testing.T) {
	rc, _, env := newStatTestRC(t)
	seedStatFile(env, "/etc/motd", "hi", "644", "root", "root")
	rc.CheckMode = true // 只读模块：check 模式行为一致
	mod := &StatModule{}
	r := mod.Run(rc, map[string]any{"path": "/etc/motd"}, "")
	f := statFacts(t, r)
	if f["exists"] != true || f["checksum"] != sha256hex([]byte("hi")) {
		t.Fatalf("check 模式应同样采集: %+v", f)
	}

	// 缺参
	if r := mod.Run(rc, nil, ""); !r.Failed {
		t.Fatal("缺 path 应失败")
	}
}
