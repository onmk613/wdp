package module

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// buildTarGz 构造真实 tar.gz（entries: 归档内路径 → 内容；二进制条目 0755）。
// 排序写入：tar 内条目顺序确定，「同名第一个命中」类断言不随 map 迭代序漂移。
func buildTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := entries[name]
		mode := int64(0o644)
		if filepath.Base(name) != "README.md" {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// artifactSrv 起一个服务归档字节并计数命中的 HTTP 服务。
func artifactSrv(t *testing.T, body []byte) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestArtifactMembersFlow(t *testing.T) {
	arc := buildTarGz(t, map[string]string{
		"kubernetes/server/bin/kubelet":   "KUBELET-BIN",
		"kubernetes/server/bin/kubectl":   "KUBECTL-BIN",
		"kubernetes/server/bin/README.md": "docs",
	})
	chartDir := t.TempDir()
	srv, hits := artifactSrv(t, arc)

	rc, fake := newTestRC(t)
	rc.BaseDir = chartDir
	mod := &ArtifactModule{}
	args := map[string]any{
		"url":     srv.URL + "/kubernetes-server.tar.gz",
		"cache":   "packages/x86_64/kubernetes-server.tar.gz",
		"members": []any{"kubelet", "kubectl"},
		"dest":    "/usr/bin",
	}

	// 首次：cache 未命中 → 下载落缓存 → 成员拍平分发
	r1 := mod.Run(rc, args, "")
	if r1.Failed {
		t.Fatalf("首次分发失败: %s", r1.Msg)
	}
	if !r1.Changed {
		t.Fatal("首次分发应 changed")
	}
	if got, _ := fake.File("/usr/bin/kubelet"); got != "KUBELET-BIN" {
		t.Fatalf("kubelet 内容 %q", got)
	}
	if got, _ := fake.File("/usr/bin/kubectl"); got != "KUBECTL-BIN" {
		t.Fatalf("kubectl 内容 %q", got)
	}
	if _, exists := fake.File("/usr/bin/README.md"); exists {
		t.Fatal("未选取的成员不应分发")
	}
	if fake.Modes["/usr/bin/kubelet"].Perm() != 0o755 {
		t.Fatalf("应沿用归档条目权限 0755: %v", fake.Modes["/usr/bin/kubelet"])
	}
	if *hits != 1 {
		t.Fatalf("应恰好下载 1 次，实际 %d", *hits)
	}
	if _, err := os.Stat(filepath.Join(chartDir, "packages/x86_64/kubernetes-server.tar.gz")); err != nil {
		t.Fatalf("缓存未落盘: %v", err)
	}

	// 二次：cache 命中 → 零下载，远端校验和一致 → 不变更
	r2 := mod.Run(rc, args, "")
	if r2.Failed {
		t.Fatalf("二次分发失败: %s", r2.Msg)
	}
	if r2.Changed {
		t.Fatal("幂等重跑不应 changed")
	}
	if *hits != 1 {
		t.Fatalf("cache 命中不应再次下载，实际 %d 次", *hits)
	}
	if !bytes.Contains([]byte(r2.Msg), []byte("cache hit")) {
		t.Fatalf("消息应说明 cache hit: %s", r2.Msg)
	}

	// 未命中的成员名必须报错（拼错文件名当场失败）
	bad := map[string]any{
		"url": srv.URL, "cache": "packages/x86_64/kubernetes-server.tar.gz",
		"members": []any{"kubelet", "no-such-bin"}, "dest": "/usr/bin",
	}
	if r := mod.Run(rc, bad, ""); !r.Failed || !bytes.Contains([]byte(r.Msg), []byte("no-such-bin")) {
		t.Fatalf("未命中成员应报错: %+v", r)
	}
}

func TestArtifactOfflineMissingFails(t *testing.T) {
	rc, _ := newTestRC(t)
	rc.BaseDir = t.TempDir() // 无缓存
	mod := &ArtifactModule{}
	// cache 缺失且无 url：离线制品不齐，明确失败而非静默联网
	r := mod.Run(rc, map[string]any{"cache": "packages/x86_64/app.tgz", "dest": "/usr/bin", "members": []any{"app"}}, "")
	if !r.Failed || !bytes.Contains([]byte(r.Msg), []byte("offline artifact")) {
		t.Fatalf("离线缺失应失败: %+v", r)
	}
	// check 模式：cache 未命中只预估，不下载不落盘
	rc2, _ := newTestRC(t)
	rc2.BaseDir = t.TempDir()
	rc2.CheckMode = true
	r = mod.Run(rc2, map[string]any{"url": "http://x/app.tgz", "cache": "packages/x86_64/app.tgz", "dest": "/usr/bin", "members": []any{"app"}}, "")
	if r.Failed || !r.Changed || !bytes.Contains([]byte(r.Msg), []byte("[check]")) {
		t.Fatalf("check 应预估下载: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(rc2.BaseDir, "packages/x86_64/app.tgz")); !os.IsNotExist(err) {
		t.Fatal("check 模式不应落缓存")
	}
}

func TestArtifactPlainFileAndChecksum(t *testing.T) {
	srv, hits := artifactSrv(t, []byte("BINARY-V1"))
	chartDir := t.TempDir()
	rc, fake := newTestRC(t)
	rc.BaseDir = chartDir
	mod := &ArtifactModule{}
	args := map[string]any{"url": srv.URL + "/app", "cache": "packages/x86_64/app", "dest": "/usr/local/bin/app"}

	if r := mod.Run(rc, args, ""); r.Failed || !r.Changed {
		t.Fatalf("单文件分发: %+v", r)
	}
	if got, _ := fake.File("/usr/local/bin/app"); got != "BINARY-V1" {
		t.Fatalf("内容 %q", got)
	}
	if fake.Modes["/usr/local/bin/app"].Perm() != 0o755 {
		t.Fatalf("缺省权限应 0755: %v", fake.Modes["/usr/local/bin/app"])
	}

	// sha256 不符（url 内容与声明不符）：下载后校验失败，不落缓存
	wrong := map[string]any{"url": srv.URL + "/app", "cache": "packages/x86_64/other", "dest": "/tmp/o", "sha256": sha256hex([]byte("OTHER"))}
	if r := mod.Run(rc, wrong, ""); !r.Failed || !bytes.Contains([]byte(r.Msg), []byte("checksum mismatch")) {
		t.Fatalf("校验失败: %+v", r)
	}

	// 缓存损坏（sha256 不符）且有 url：自愈重下
	srv2, _ := artifactSrv(t, []byte("BINARY-V2"))
	if err := os.MkdirAll(filepath.Join(chartDir, "packages/x86_64"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chartDir, "packages/x86_64/app"), []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := mod.Run(rc, map[string]any{
		"url": srv2.URL + "/app", "cache": "packages/x86_64/app", "dest": "/usr/local/bin/app",
		"sha256": sha256hex([]byte("BINARY-V2")),
	}, "")
	if r.Failed {
		t.Fatalf("自愈重下失败: %s", r.Msg)
	}
	if got, _ := fake.File("/usr/local/bin/app"); got != "BINARY-V2" {
		t.Fatalf("自愈后内容 %q", got)
	}
	_ = hits
}

func TestSelectArchiveMembers(t *testing.T) {
	arc := buildTarGz(t, map[string]string{
		"top/README":         "r",
		"top/bin/app":        "A",
		"top/bin/nested/app": "NESTED-DUP",
		"top/other/app":      "OTHER-DUP",
		"top/docs/guide.md":  "g",
	})
	// basename 检索 + 相对路径精确 + 未命中报错 + 同名首个命中
	sel, err := selectArchiveMembers("targz", arc, []string{"app", "top/docs/guide.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) != 2 {
		t.Fatalf("选取数 %d: %+v", len(sel), sel)
	}
	if string(sel[0].data) != "A" || sel[0].mode != 0o755 {
		t.Fatalf("app 成员: %+v", sel[0])
	}
	if string(sel[1].data) != "g" {
		t.Fatalf("guide 成员: %+v", sel[1])
	}
	if _, err := selectArchiveMembers("targz", arc, []string{"missing-bin"}); err == nil ||
		!bytes.Contains([]byte(err.Error()), []byte("missing-bin")) {
		t.Fatalf("未命中应报错: %v", err)
	}
	// zip 支持
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("etcd-v3.5/linux/etcdctl")
	_, _ = w.Write([]byte("ETCDCTL"))
	_ = zw.Close()
	sel, err = selectArchiveMembers("zip", zbuf.Bytes(), []string{"etcdctl"})
	if err != nil || len(sel) != 1 || string(sel[0].data) != "ETCDCTL" {
		t.Fatalf("zip 选取: %v %+v", err, sel)
	}
	// 不支持的格式
	if _, err := selectArchiveMembers("tarxz", arc, []string{"app"}); err == nil {
		t.Fatal("xz 成员选取应报不支持")
	}
}

func TestUnarchiveMembers(t *testing.T) {
	arc := buildTarGz(t, map[string]string{
		"kubernetes/server/bin/kubelet": "KUBELET",
		"kubernetes/server/bin/kubectl": "KUBECTL",
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.tar.gz"), arc, 0o644); err != nil {
		t.Fatal(err)
	}
	rc, fake := newTestRC(t)
	rc.BaseDir = dir
	mod := &UnarchiveModule{}
	args := map[string]any{"src": "server.tar.gz", "dest": "/opt/bin", "members": []any{"kubelet", "kubectl"}}

	r1 := mod.Run(rc, args, "")
	if r1.Failed {
		t.Fatalf("成员解压失败: %s", r1.Msg)
	}
	if !r1.Changed {
		t.Fatal("首次应 changed")
	}
	if got, _ := fake.File("/opt/bin/kubelet"); got != "KUBELET" {
		t.Fatalf("kubelet: %q", got)
	}
	// 幂等：远端校验和一致
	r2 := mod.Run(rc, args, "")
	if r2.Failed || r2.Changed {
		t.Fatalf("重跑应幂等: %+v", r2)
	}
	// 未命中成员报错
	if r := mod.Run(rc, map[string]any{"src": "server.tar.gz", "dest": "/opt/bin", "members": []any{"nope"}}, ""); !r.Failed {
		t.Fatal("未命中成员应失败")
	}
	// remote_src 与 members 互斥
	if r := mod.Run(rc, map[string]any{"src": "/tmp/s.tar.gz", "dest": "/opt/bin", "members": []any{"kubelet"}, "remote_src": true}, ""); !r.Failed {
		t.Fatal("remote_src + members 应失败")
	}
}
