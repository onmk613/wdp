package module

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/connection"
)

// arcShell 模拟 unarchive 模块用到的远端 sh 命令（存在性/解压/清理）。
type arcShell struct {
	fake  *connection.Fake
	files map[string]bool // exec 层可见的远端路径
	dirs  map[string]bool
	cmds  map[string]bool // 目标机可用命令（unzip 等）
	runs  []string        // 变更类命令记录（解压/建目录/清理）
}

func newUnarchiveRC(t *testing.T) (*RunContext, *connection.Fake, *arcShell) {
	t.Helper()
	rc, fake := newTestRC(t)
	sh := &arcShell{
		fake:  fake,
		files: map[string]bool{},
		dirs:  map[string]bool{},
		cmds:  map[string]bool{"tar": true},
	}
	fake.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, "command -v unzip"):
			if sh.cmds["unzip"] {
				return connection.ExecResult{Code: 0}, nil
			}
			return connection.ExecResult{Code: 1}, nil
		case strings.Contains(s, "[ -L"): // probePath
			path := extractQuoted(s, "p=")
			switch {
			case sh.dirs[path]:
				return connection.ExecResult{Code: 0, Stdout: "directory\n"}, nil
			case sh.files[path]:
				return connection.ExecResult{Code: 0, Stdout: "file\n"}, nil
			default:
				return connection.ExecResult{Code: 0, Stdout: "missing\n"}, nil
			}
		case strings.Contains(s, "[ -e"):
			path := firstQuoted(s)
			if sh.files[path] || sh.dirs[path] {
				return connection.ExecResult{Code: 0}, nil
			}
			return connection.ExecResult{Code: 1}, nil
		case strings.Contains(s, "mkdir -p"):
			sh.dirs[firstQuoted(s)] = true
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "tar -x"), strings.Contains(s, "unzip -o"):
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "rm -f"):
			delete(sh.files, firstQuoted(s))
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		default:
			return connection.ExecResult{Code: 0}, nil
		}
	}
	return rc, fake, sh
}

// writeArchive 写本地归档文件并返回相对名。
func writeArchive(t *testing.T, name string, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUnarchiveLocalTar(t *testing.T) {
	dir := writeArchive(t, "app.tar.gz", "fake-tar-bytes")
	rc, fake, sh := newUnarchiveRC(t)
	rc.BaseDir = dir
	mod := &UnarchiveModule{}

	r := mod.Run(rc, map[string]any{"src": "app.tar.gz", "dest": "/opt/app", "creates": "/opt/app/bin/app"}, "")
	if r.Failed {
		t.Fatalf("解压失败: %s", r.Msg)
	}
	if !r.Changed {
		t.Fatal("首次解压应 changed")
	}
	// 应上传到 /tmp/.wdp-arc-* 临时路径
	found := false
	for p := range fake.Files {
		if strings.HasPrefix(p, "/tmp/.wdp-arc-") {
			if got, _ := fake.File(p); got != "fake-tar-bytes" {
				t.Fatalf("上传内容 %q", got)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("未见临时归档上传")
	}
	// mkdir + tar + defer 清理临时归档（临时副本总是自动清理）
	if len(sh.runs) != 3 || !strings.Contains(sh.runs[0], "mkdir -p -- '/opt/app'") {
		t.Fatalf("执行记录: %v", sh.runs)
	}
	if sh.runs[1] != "tar -xzf '"+tempArcPath(fake)+"' -C '/opt/app'" {
		t.Fatalf("解压命令: %q", sh.runs[1])
	}
	if sh.runs[2] != "rm -f -- '"+tempArcPath(fake)+"'" {
		t.Fatalf("应自动清理临时归档: %q", sh.runs[2])
	}

	// creates 出现后应跳过（幂等）
	sh.files["/opt/app/bin/app"] = true
	r = mod.Run(rc, map[string]any{"src": "app.tar.gz", "dest": "/opt/app", "creates": "/opt/app/bin/app"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("creates 已存在应跳过: %+v", r)
	}
	if !strings.Contains(r.Msg, "跳过") {
		t.Fatalf("消息 %q", r.Msg)
	}
}

// tempArcPath 取出上传的临时归档路径。
func tempArcPath(fake *connection.Fake) string {
	for p := range fake.Files {
		if strings.HasPrefix(p, "/tmp/.wdp-arc-") {
			return p
		}
	}
	return ""
}

func TestUnarchiveRemoteSrcZip(t *testing.T) {
	rc, _, sh := newUnarchiveRC(t)
	mod := &UnarchiveModule{}
	args := map[string]any{"src": "/tmp/data.zip", "dest": "/srv/data", "remote_src": true}
	sh.files["/tmp/data.zip"] = true

	// unzip 未安装：明确报错
	r := mod.Run(rc, args, "")
	if !r.Failed || !strings.Contains(r.Msg, "unzip") {
		t.Fatalf("缺 unzip 应失败: %+v", r)
	}

	// 安装 unzip 后正常解压
	sh.cmds["unzip"] = true
	r = mod.Run(rc, args, "")
	if r.Failed || !r.Changed {
		t.Fatalf("zip 解压: %+v", r)
	}
	last := sh.runs[len(sh.runs)-1]
	if last != "unzip -o '/tmp/data.zip' -d '/srv/data'" {
		t.Fatalf("解压命令: %q", last)
	}

	// 远端归档缺失应失败
	r = mod.Run(rc, map[string]any{"src": "/tmp/none.zip", "dest": "/srv/x", "remote_src": true}, "")
	if !r.Failed || !strings.Contains(r.Msg, "不存在") {
		t.Fatalf("归档缺失应失败: %+v", r)
	}
}

func TestUnarchiveRemoveCleansTemp(t *testing.T) {
	dir := writeArchive(t, "pkg.tar", "tar")
	rc, fake, sh := newUnarchiveRC(t)
	rc.BaseDir = dir
	mod := &UnarchiveModule{}

	r := mod.Run(rc, map[string]any{"src": "pkg.tar", "dest": "/opt/pkg", "remove": true}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("remove 解压: %+v", r)
	}
	var rmCmd string
	for _, c := range sh.runs {
		if strings.HasPrefix(c, "rm -f") {
			rmCmd = c
		}
	}
	if rmCmd != "rm -f -- '"+tempArcPath(fake)+"'" {
		t.Fatalf("应清理临时归档: %q (runs=%v)", rmCmd, sh.runs)
	}

	// remove + remote_src 不允许
	r2 := mod.Run(rc, map[string]any{"src": "/tmp/a.tar", "dest": "/x", "remote_src": true, "remove": true}, "")
	if !r2.Failed {
		t.Fatal("remote_src + remove 应失败")
	}
}

func TestUnarchiveCheckMode(t *testing.T) {
	dir := writeArchive(t, "app.tgz", "x")
	rc, fake, sh := newUnarchiveRC(t)
	rc.BaseDir = dir
	rc.CheckMode = true
	rc.DiffMode = true
	mod := &UnarchiveModule{}

	r := mod.Run(rc, map[string]any{"src": "app.tgz", "dest": "/opt/app"}, "")
	if r.Failed {
		t.Fatalf("check 失败: %s", r.Msg)
	}
	if !r.Changed || !strings.Contains(r.Msg, "[check] 将解压") {
		t.Fatalf("check 预估: %+v", r)
	}
	if r.Diff == "" || !strings.Contains(r.Diff, "+ /opt/app") {
		t.Fatalf("diff: %q", r.Diff)
	}
	if len(sh.runs) != 0 {
		t.Fatalf("check 不应有变更命令: %v", sh.runs)
	}
	if len(fake.Files) != 0 {
		t.Fatal("check 不应上传文件")
	}
	// check 模式 + creates 已存在 → 无变更
	rc2, _, sh2 := newUnarchiveRC(t)
	rc2.CheckMode = true
	sh2.files["/opt/app/.done"] = true
	r2 := mod.Run(rc2, map[string]any{"src": dir + "/app.tgz", "dest": "/opt/app", "creates": "/opt/app/.done"}, "")
	if r2.Failed || r2.Changed {
		t.Fatalf("creates 已存在时 check 不应变更: %+v", r2)
	}
}

func TestUnarchiveRollbackRegister(t *testing.T) {
	dir := writeArchive(t, "app.tar.gz", "x")
	rc, _, _ := newUnarchiveRC(t)
	rc.BaseDir = dir
	var actions []RollbackAction
	rc.Rollback = &RollbackCtx{Dir: "/var/.wdp-rollback", Record: func(a RollbackAction) { actions = append(actions, a) }}

	r := (&UnarchiveModule{}).Run(rc, map[string]any{"src": "app.tar.gz", "dest": "/opt/app"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("%+v", r)
	}
	if len(actions) != 1 || actions[0] != (RollbackAction{Kind: "remove", Path: "/opt/app"}) {
		t.Fatalf("回滚登记: %+v", actions)
	}
	// 已存在的 dest 不登记删除
	rc2, _, sh2 := newUnarchiveRC(t)
	rc2.BaseDir = dir
	sh2.dirs["/opt/app"] = true
	actions = nil
	rc2.Rollback = &RollbackCtx{Dir: "/var/.wdp-rollback", Record: func(a RollbackAction) { actions = append(actions, a) }}
	r2 := (&UnarchiveModule{}).Run(rc2, map[string]any{"src": "app.tar.gz", "dest": "/opt/app"}, "")
	if r2.Failed || len(actions) != 0 {
		t.Fatalf("dest 已存在不登记回滚: %+v actions=%v", r2, actions)
	}
}

func TestUnarchiveValidation(t *testing.T) {
	rc, _, _ := newUnarchiveRC(t)
	mod := &UnarchiveModule{}
	if r := mod.Run(rc, map[string]any{"dest": "/x"}, ""); !r.Failed {
		t.Fatal("缺 src 应失败")
	}
	if r := mod.Run(rc, map[string]any{"src": "a.rar", "dest": "/x"}, ""); !r.Failed || !strings.Contains(r.Msg, "格式") {
		t.Fatalf("未知格式: %+v", r)
	}
	// dest 已存在但不是目录
	rc2, _, sh2 := newUnarchiveRC(t)
	sh2.files["/opt/app"] = true
	dir := writeArchive(t, "a.tar", "x")
	r := mod.Run(rc2, map[string]any{"src": dir + "/a.tar", "dest": "/opt/app"}, "")
	if !r.Failed || !strings.Contains(r.Msg, "不是目录") {
		t.Fatalf("dest 非目录: %+v", r)
	}
}

// nativeConn 包装假连接并实现 NativeExtractor（模拟 agent/push 通道）。
type nativeConn struct {
	*connection.Fake
	calls []string
	err   error
}

func (n *nativeConn) NativeExtract(ctx context.Context, src, dest string) error {
	n.calls = append(n.calls, src+"→"+dest)
	return n.err
}

// TestUnarchiveNativePreferred 原生路径优先：目标机没有 unzip 也能解 zip，
// 且不触发任何 shell 解压命令。
func TestUnarchiveNativePreferred(t *testing.T) {
	dir := writeArchive(t, "app.zip", "zip-bytes")
	rc, fake, sh := newUnarchiveRC(t)
	sh.cmds["unzip"] = false // 目标机没有 unzip
	rc.BaseDir = dir
	nc := &nativeConn{Fake: fake}
	rc.Conn = nc

	r := (&UnarchiveModule{}).Run(rc, map[string]any{"src": "app.zip", "dest": "/opt/app"}, "")
	if r.Failed {
		t.Fatalf("原生解压应成功: %s", r.Msg)
	}
	if len(nc.calls) != 1 || !strings.Contains(nc.calls[0], "→/opt/app") {
		t.Fatalf("应恰好一次原生解压: %v", nc.calls)
	}
	for _, s := range sh.runs {
		if strings.Contains(s, "unzip") || strings.Contains(s, "tar -x") {
			t.Fatalf("不应触发 shell 解压: %v", sh.runs)
		}
	}
	if !strings.Contains(r.Msg, "原生") {
		t.Fatalf("消息应标注原生: %q", r.Msg)
	}
}

// TestUnarchiveNativeUnsupportedFallsBack 旧版 agent 哨兵 → 回退 shell unzip。
func TestUnarchiveNativeUnsupportedFallsBack(t *testing.T) {
	dir := writeArchive(t, "app.zip", "zip-bytes")
	rc, fake, sh := newUnarchiveRC(t)
	sh.cmds["unzip"] = true
	rc.BaseDir = dir
	nc := &nativeConn{Fake: fake, err: connection.ErrNativeUnsupported}
	rc.Conn = nc

	r := (&UnarchiveModule{}).Run(rc, map[string]any{"src": "app.zip", "dest": "/opt/app"}, "")
	if r.Failed {
		t.Fatalf("回退解压应成功: %s", r.Msg)
	}
	found := false
	for _, s := range sh.runs {
		if strings.Contains(s, "unzip -o") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应回退 shell unzip: %v", sh.runs)
	}
}

// TestUnarchiveNativeErrorFailsLoud 原生解压真实失败（非哨兵）不回退、显式上抛。
func TestUnarchiveNativeErrorFailsLoud(t *testing.T) {
	dir := writeArchive(t, "app.tar.gz", "bytes")
	rc, fake, sh := newUnarchiveRC(t)
	rc.BaseDir = dir
	nc := &nativeConn{Fake: fake, err: errors.New("归档损坏")}
	rc.Conn = nc

	r := (&UnarchiveModule{}).Run(rc, map[string]any{"src": "app.tar.gz", "dest": "/opt/app"}, "")
	if !r.Failed || !strings.Contains(r.Msg, "归档损坏") {
		t.Fatalf("真实失败应上抛: %+v", r)
	}
	for _, s := range sh.runs {
		if strings.Contains(s, "tar -x") {
			t.Fatalf("不应回退 shell: %v", sh.runs)
		}
	}
}
