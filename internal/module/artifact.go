package module

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"wdp/internal/fsatomic"
	"wdp/internal/i18n"
)

func init() {
	Register(&ArtifactModule{})
}

// ArtifactModule 是离线优先的制品分发：单个任务统一在线/离线两种来源。
//
// 语义 cache-first：控制机 cache 路径已有制品（预置离线包 / 前次下载）则
// 永不联网，直接分发；未命中且有 url 时在控制端下载一次并落缓存（下次及
// 全部主机复用），再分发。online/offline 不再是两套任务。
//
// cache 缺失且未给 url 时直接失败——离线制品不齐必须在任务期暴露，而不是
// 静默回退到联网下载（那正是离线环境会挂掉的地方）。
type ArtifactModule struct{}

func (m *ArtifactModule) Name() string { return "artifact" }

// RollbackCapability 变更经快照登记可自动回滚（与 copy 相同的 putFile 管线）。
func (m *ArtifactModule) RollbackCapability() RollbackCapability { return RollbackFull }

func (m *ArtifactModule) Desc() string {
	return i18n.T("Distribute artifacts to target hosts; controller cache first (a hit works offline, a miss downloads once into the cache)", "分发制品到目标机；控制端缓存优先（命中即离线可用，未命中下载一次入缓存）")
}

func (m *ArtifactModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "cache", Type: "string", Desc: i18n.T("controller cache path (relative path inside the chart, e.g. packages/<arch>/app.tgz); a non-empty file at this path is used as is and never downloaded (required)", "控制端缓存路径（chart 内相对路径，如 packages/<arch>/app.tgz）；该路径存在非空文件即直接使用、永不下载（必填）")},
		{Name: "dest", Type: "string", Desc: i18n.T("remote destination: a file path when members is omitted, a directory when members is given (required)", "远端目标：不带 members 时是文件路径，带 members 时是目录（必填）")},
		{Name: "url", Type: "string", Desc: i18n.T("online source the controller downloads into the cache on a miss (http/https); fails when the cache is missing and no url is given", "在线源，缓存未命中时由控制端下载入缓存（http/https）；缓存缺失且无 url 则失败")},
		{Name: "members", Type: "list", Desc: i18n.T("archive entries picked by name (basename or full path inside the archive, flattened into dest/); distributes the whole artifact by default", "按名字挑选的压缩包条目（basename 或完整包内路径，拍平进 dest/）；缺省分发整个制品")},
		{Name: "mode", Type: "mode", Default: "0755", Desc: i18n.T("mode on the target (when unset, members keep their archive entry permissions)", "目标权限位（不设时 members 保持包内条目权限）")},
		{Name: "sha256", Type: "string", Desc: i18n.T("expected sha256 (verified for both cache hits and downloads; re-downloads when the cache mismatches and url is given)", "期望 sha256（缓存命中与下载都校验；缓存不匹配且给了 url 时重新下载）")},
		{Name: "timeout_secs", Type: "int", Default: "30", Desc: i18n.T("download timeout in seconds (controller)", "下载超时（秒，控制端）")},
		{Name: "headers", Type: "map", Desc: i18n.T("extra request headers (e.g. Authorization)", "附加请求头（如 Authorization）")},
	}
}

func (m *ArtifactModule) Example() string {
	return i18n.T(`# One task for online and offline: a pre-seeded packages/ cache -> works offline;
# an empty cache -> the controller downloads once, then distributes
- name: Distribute Kubernetes components from the official archive
  artifact:
    url: "https://dl.k8s.io/v1.31.0/kubernetes-server-linux-amd64.tar.gz"
    cache: "packages/{{ .arch }}/kubernetes-server-{{ .version }}.tar.gz"
    members: [kube-apiserver, kube-controller-manager, kube-scheduler, kubelet, kubectl]
    dest: "{{ .bin_dir }}"
    mode: "0755"

# bare binary artifact (not an archive): dest is a file path
- name: Distribute etcdctl
  artifact:
    url: "https://github.com/etcd-io/etcd/releases/download/{{ .etcd_version }}/etcd-{{ .etcd_version }}-linux-{{ .arch }}.tar.gz"
    cache: "packages/{{ .arch }}/etcd-{{ .etcd_version }}.tar.gz"
    members: [etcdctl]
    dest: "{{ .bin_dir }}"
    mode: "0755"

# an empty cache -> the controller downloads once, then distributes
- name: Distribute Kubernetes components from the official archive
  artifact:
    url: "https://dl.k8s.io/v1.31.0/kubernetes-server-linux-amd64.tar.gz"
    cache: "packages/{{ .arch }}/kubernetes-server-{{ .version }}.tar.gz"
    members: [kube-apiserver, kube-controller-manager, kube-scheduler, kubelet, kubectl]
    dest: "{{ .bin_dir }}"
    mode: "0755"

# bare binary artifact (not an archive): dest is a file path
- name: Distribute etcdctl
  artifact:
    url: "https://github.com/etcd-io/etcd/releases/download/{{ .etcd_version }}/etcd-{{ .etcd_version }}-linux-{{ .arch }}.tar.gz"
    cache: "packages/{{ .arch }}/etcd-{{ .etcd_version }}.tar.gz"
    members: [etcdctl]
    dest: "{{ .bin_dir }}"
    mode: "0755"
`, `# 在线/离线一份任务：packages/ 预置了缓存 -> 离线可用；
# 缓存为空 -> 控制端下载一次，之后分发
- name: 从官方包分发 Kubernetes 组件
  artifact:
    url: "https://dl.k8s.io/v1.31.0/kubernetes-server-linux-amd64.tar.gz"
    cache: "packages/{{ .arch }}/kubernetes-server-{{ .version }}.tar.gz"
    members: [kube-apiserver, kube-controller-manager, kube-scheduler, kubelet, kubectl]
    dest: "{{ .bin_dir }}"
    mode: "0755"

# 裸二进制制品（非压缩包）：dest 为文件路径
- name: 分发 etcdctl
  artifact:
    url: "https://github.com/etcd-io/etcd/releases/download/{{ .etcd_version }}/etcd-{{ .etcd_version }}-linux-{{ .arch }}.tar.gz"
    cache: "packages/{{ .arch }}/etcd-{{ .etcd_version }}.tar.gz"
    members: [etcdctl]
    dest: "{{ .bin_dir }}"
    mode: "0755"
`)
}

// Run 执行制品分发。
func (m *ArtifactModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	cache, ok := argStr(args, "cache")
	if !ok || cache == "" {
		return Fail("artifact requires a cache parameter")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("artifact requires a dest parameter")
	}
	url, _ := argStr(args, "url")
	members, _ := argStrList(args, "members")
	wantSum, _ := argStr(args, "sha256")
	wantSum = strings.ToLower(strings.TrimSpace(wantSum))
	if wantSum != "" && !isSHA256Hex(wantSum) {
		return Fail("sha256 parameter must be a 64-character hex string")
	}
	timeoutSecs, ok := argSecs(args, "timeout_secs", 30)
	if !ok || timeoutSecs <= 0 {
		return Fail("timeout_secs must be a positive integer")
	}
	headers, bad := headerMapArg(args, "headers")
	if bad != nil {
		return bad
	}
	mode, hasMode := argMode(args, "mode")
	if !hasMode {
		mode = 0o755 // 制品以可执行文件为主，缺省 0755（members 沿用归档权限）
	}

	cachePath, cerr := resolveLocal(rc, cache)
	if cerr != nil {
		return Fail("%v", cerr)
	}
	data, cacheHit, res := m.loadCache(rc, cachePath, url, wantSum, headers, timeoutSecs)
	if res != nil {
		return res
	}

	if len(members) > 0 {
		return m.distributeMembers(rc, data, cacheHit, cache, dest, members, int64(mode.Perm()), hasMode)
	}

	changed, res := putFile(rc, putFileOpts{data: data, dest: dest, mode: &mode})
	if res != nil {
		return res
	}
	return &Result{Changed: changed, Msg: m.msg(cacheHit, cache, changed,
		fmt.Sprintf("%s content is unchanged", dest), fmt.Sprintf("distributed %d bytes to %s", len(data), dest))}
}

// loadCache 解析制品来源：cache 命中（含 sha256 校验通过）直接用；未命中
// 或校验失败时经 url 下载并原子落缓存。check 模式不产生控制端写入，只预估。
func (m *ArtifactModule) loadCache(rc *RunContext, cachePath, url, wantSum string, headers map[string]string, timeoutSecs int) ([]byte, bool, *Result) {
	// 读缓存走上限口径：裸 os.ReadFile 会让被替换/超大的缓存文件直接把
	// 控制端内存吃满（缓存目录可写者等于控制端 OOM 开关）
	data, rerr := readLocalCap(rc, cachePath, rc.MaxUploadBytes)
	if rerr == nil && len(data) > 0 {
		if wantSum == "" || sha256hex(data) == wantSum {
			return data, true, nil
		}
		// 缓存损坏/版本漂移：有 url 时自愈重下，无 url 则失败（离线包被动过）
		if url == "" {
			return nil, false, Fail("cached artifact %s fails the sha256 check and no url is set to refresh it", cachePath)
		}
	} else if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && url == "" {
		// 存在但读不了（含超限）：无 url 可自愈时报真实原因，别伪装成"缺包"
		return nil, false, Fail("failed to read cached artifact %s: %v", cachePath, rerr)
	}
	if rc.CheckMode {
		// check 只读：不下载不落缓存（下载落盘是控制端变更）
		return nil, false, &Result{Changed: true, Msg: fmt.Sprintf("[check] cache miss: would download %s into %s and distribute", url, cachePath)}
	}
	if url == "" {
		return nil, false, Fail("offline artifact %s is missing (prepare it with the chart's download phase or ship it inside the package; no url is set to fetch it)", cachePath)
	}
	// singleflight：forks 并发下同一制品在 N 台主机同时 miss 时，首个下载
	// 完成前其余 worker 全部重复下载同一 URL（大制品 + 高 forks 浪费带宽
	// 且可能打满连接）。键取缓存路径（同一制品多来源仍只落一份）。等待者
	// 共享先行者的下载/校验/落缓存结果；先行者的 ctx 取消会连带等待者
	// 失败——同制品同源，即刻可见优于各自重复下载放大故障。
	data, ferr := artifactFlights.do(cachePath, func() ([]byte, error) {
		data, bad := (&GetURLModule{}).fetch(rc, url, headers, timeoutSecs)
		if bad != nil {
			return nil, errors.New(bad.Msg)
		}
		if wantSum != "" && sha256hex(data) != wantSum {
			return nil, fmt.Errorf("download checksum mismatch for %s (expected %s)", url, wantSum)
		}
		if err := writeCache(cachePath, data); err != nil {
			return nil, fmt.Errorf("failed to write artifact cache %s: %v", cachePath, err)
		}
		return data, nil
	})
	if ferr != nil {
		return nil, false, Fail("%v", ferr)
	}
	return data, false, nil
}

// flightGroup 是 singleflight 语义的最小实现（键粒度的在飞去重）：同键
// 并发调用只有一个真正执行 fn，其余等待并共享同一份结果。结果视为只读
// （调用方不得改写返回的 []byte）；不加依赖库（x/sync）是为保住 go.mod
// 的最小直接依赖面。
type flightGroup struct {
	mu       sync.Mutex
	inflight map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	data []byte
	err  error
}

func (g *flightGroup) do(key string, fn func() ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	if g.inflight == nil {
		g.inflight = make(map[string]*flightCall)
	}
	if c, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.data, c.err
	}
	c := &flightCall{done: make(chan struct{})}
	g.inflight[key] = c
	g.mu.Unlock()

	c.data, c.err = fn()
	close(c.done) // 先关 channel 再出表：等待者拿结果不经过锁，删表晚无碍

	g.mu.Lock()
	delete(g.inflight, key)
	g.mu.Unlock()
	return c.data, c.err
}

// artifactFlights 是制品下载的进程级在飞表（Executor 各批次共用）。
var artifactFlights flightGroup

// distributeMembers 从归档制品中选取成员并逐个分发（拍平到 dest/，basename
// 命中；权限沿用归档条目，mode 参数显式给出时强制覆盖）。
func (m *ArtifactModule) distributeMembers(rc *RunContext, data []byte, cacheHit bool, cache, dest string, members []string, forcedMode int64, hasMode bool) *Result {
	kind := archiveKind(cache)
	sel, err := selectArchiveMembers(kind, data, members, rc.MaxUploadBytes)
	if err != nil {
		return Fail("artifact %s: %v", cache, err)
	}
	cur, bad := destDirState(rc, dest)
	if bad != nil {
		return bad
	}
	// 新建目录登记回滚删除（RollbackFull 声明含目录本身；此前 members
	// 路径漏登记，auto_rollback 后残留空目录）
	recordMkdirRollback(rc, dest, cur)
	// check 模式不得产生任何目标机变更：目录缺失时只做"将会创建"的预估
	//（逐成员 putFile 在 check 下不落盘，与 unarchive 同一约定）
	if bad := mkdirDestMissing(rc, dest, cur); bad != nil {
		return bad
	}
	changed, bad := distributeMemberFiles(rc, dest, sel, forcedMode, hasMode)
	if bad != nil {
		return bad
	}
	return &Result{Changed: changed, Msg: m.msg(cacheHit, cache, changed,
		fmt.Sprintf("%d members from %s are unchanged in %s", len(sel), cache, dest),
		fmt.Sprintf("distributed %d members from %s to %s", len(sel), cache, dest))}
}

// writeCache 原子落缓存（临时名 + rename，并发执行不会留半成品）。落盘
// 走 fsatomic（fsync + 目录 fsync）——与 executor/plan 的 writeFileDurably
// 同一耐久口径：裸 rename 在掉电后可能产出空壳缓存（下次运行由 sha256
// 校验自愈，但没必要留这个窗口）。
func writeCache(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsatomic.WriteFile(path, bytes.NewReader(data), 0o644)
}

// msg 组装来源说明（cache 命中 = 离线复用；未命中 = 在线下载落缓存）。
func (m *ArtifactModule) msg(cacheHit bool, cache string, changed bool, unchanged, distributed string) string {
	source := "downloaded into cache"
	if cacheHit {
		source = "cache hit"
	}
	if !changed {
		return fmt.Sprintf("%s (%s: %s)", unchanged, source, cache)
	}
	return fmt.Sprintf("%s (%s: %s)", distributed, source, cache)
}
