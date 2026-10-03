package module

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestArtifactCheckModeDoesNotCreateDest check 模式下 members 分发不得在
// 目标机创建任何目录（此前 cur=="missing" 时无条件 mkdir -p，破坏
// "零风险预演"承诺）。
func TestArtifactCheckModeDoesNotCreateDest(t *testing.T) {
	arc := buildTarGz(t, map[string]string{"bin/app": "APP"})
	chartDir := t.TempDir()
	cacheRel := "packages/x86_64/app.tar.gz"
	if err := os.MkdirAll(filepath.Join(chartDir, filepath.Dir(cacheRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chartDir, cacheRel), arc, 0o644); err != nil {
		t.Fatal(err)
	}

	rc, _, dirs := newTestRCState(t)
	rc.BaseDir = chartDir
	rc.CheckMode = true

	r := (&ArtifactModule{}).Run(rc, map[string]any{
		"cache": cacheRel, "members": []any{"app"}, "dest": "/opt/app",
	}, "")
	if r.Failed {
		t.Fatalf("check 预演不应失败: %+v", r)
	}
	if !r.Changed {
		t.Fatal("目标缺失时 check 应预估 changed")
	}
	if dirs["/opt/app"] {
		t.Fatal("check 模式不得创建目标目录")
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
	sel, err := selectArchiveMembers("targz", arc, []string{"app", "top/docs/guide.md"}, 0)
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
	if _, err := selectArchiveMembers("targz", arc, []string{"missing-bin"}, 0); err == nil ||
		!bytes.Contains([]byte(err.Error()), []byte("missing-bin")) {
		t.Fatalf("未命中应报错: %v", err)
	}
	// zip 支持
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("etcd-v3.5/linux/etcdctl")
	_, _ = w.Write([]byte("ETCDCTL"))
	_ = zw.Close()
	sel, err = selectArchiveMembers("zip", zbuf.Bytes(), []string{"etcdctl"}, 0)
	if err != nil || len(sel) != 1 || string(sel[0].data) != "ETCDCTL" {
		t.Fatalf("zip 选取: %v %+v", err, sel)
	}
	// 不支持的格式
	if _, err := selectArchiveMembers("tarxz", arc, []string{"app"}, 0); err == nil {
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
	// remote_src + members 已支持（目标机暂存解压拍平，行为级测试见
	// unarchive_remote_test.go）；此处只确认不再被参数层拒绝
	if r := mod.Run(rc, map[string]any{"src": "/tmp/s.tar.gz", "dest": "/opt/bin", "members": []any{"kubelet"}, "remote_src": true}, ""); r.Failed {
		t.Fatalf("remote_src + members 应可用: %s", r.Msg)
	}
}

// TestArtifactConcurrentDownloadSingleflight 并发缓存击穿回归：forks 下
// N 台主机同刻 miss 时同一制品只下载一次——此前每个 worker 各自下载
// 同一 URL（大制品 + 高 forks 浪费带宽且可能打满连接）。
func TestArtifactConcurrentDownloadSingleflight(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(300 * time.Millisecond) // 敞开并发窗口：全部调用者都处在 miss 态
		_, _ = w.Write([]byte("artifact-blob-v1"))
	}))
	t.Cleanup(srv.Close)

	cache := filepath.Join(t.TempDir(), "pkg.bin")
	const n = 6
	var wg sync.WaitGroup
	type outcome struct {
		data []byte
		res  *Result
	}
	outcomes := make([]outcome, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := &ArtifactModule{}
			data, _, res := m.loadCache(&RunContext{Ctx: context.Background(), MaxUploadBytes: 1 << 20},
				cache, srv.URL+"/pkg.bin", "", nil, 10)
			outcomes[i] = outcome{data: data, res: res}
		}()
	}
	wg.Wait()
	if got := hits.Load(); got != 1 {
		t.Fatalf("并发 miss 应只下载一次（singleflight 失效）: 实际 %d 次", got)
	}
	for i, oc := range outcomes {
		if oc.res != nil && oc.res.Failed {
			t.Fatalf("第 %d 个调用者不应失败: %s", i, oc.res.Msg)
		}
		if string(oc.data) != "artifact-blob-v1" {
			t.Fatalf("第 %d 个调用者应拿到共享结果: %q", i, oc.data)
		}
	}
	// 缓存已落盘：后续调用不再发起网络请求
	hits.Store(0)
	m := &ArtifactModule{}
	data, cacheHit, res := m.loadCache(&RunContext{Ctx: context.Background(), MaxUploadBytes: 1 << 20},
		cache, srv.URL+"/pkg.bin", "", nil, 10)
	if res != nil && res.Failed || !cacheHit || string(data) != "artifact-blob-v1" {
		t.Fatalf("缓存应命中: hit=%v data=%q res=%+v", cacheHit, data, res)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("缓存命中后不应再下载: %d 次", got)
	}
}

// TestFlightGroupErrorShared 等待者共享先行者的失败（同源失败即刻可见，
// 而非各自重试放大故障）；失败的键在先行者结束后可重试。
func TestFlightGroupErrorShared(t *testing.T) {
	var g flightGroup
	calls := atomic.Int32{}
	fn := func() ([]byte, error) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond)
		return nil, errors.New("boom")
	}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = g.do("k", fn)
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("失败也应只执行一次: %d", got)
	}
	for i, err := range errs {
		if err == nil || err.Error() != "boom" {
			t.Fatalf("第 %d 个等待者应共享失败: %v", i, err)
		}
	}
	// 在飞表已清理：新调用重新执行
	if _, err := g.do("k", fn); err == nil {
		t.Fatal("新一轮应重新执行并再次失败")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("在飞表应清理: %d", got)
	}
}
