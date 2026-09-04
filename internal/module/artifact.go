package module

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wdp/internal/shellquote"
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

// Name 模块名。
func (m *ArtifactModule) Name() string { return "artifact" }

// RollbackCapability 变更经快照登记可自动回滚（与 copy 相同的 putFile 管线）。
func (m *ArtifactModule) RollbackCapability() RollbackCapability { return RollbackFull }

// Desc 模块说明。
func (m *ArtifactModule) Desc() string {
	return "distribute an artifact with a control-side cache (cache hit = offline, miss = download once into the cache)"
}

// Params 参数文档。
func (m *ArtifactModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "cache", Type: "string", Desc: "control-side cache path (chart-relative, e.g. packages/<arch>/app.tgz); a non-empty file at this path is used as-is, never downloading (required)"},
		{Name: "dest", Type: "string", Desc: "remote destination: file path without members, directory with members (required)"},
		{Name: "url", Type: "string", Desc: "online source downloaded into cache on a miss (http/https, fetched from the control node); a missing cache without url fails"},
		{Name: "members", Type: "list", Desc: "archive entries to pick by name (basename or full archive path, flattened into dest/); omit to distribute the whole artifact"},
		{Name: "mode", Type: "mode", Default: "0755", Desc: "destination mode (members keep their archive entry mode unless set)"},
		{Name: "sha256", Type: "string", Desc: "expected sha256 of the artifact (verified on both cache hits and downloads; a mismatched cache re-downloads when url is set)"},
		{Name: "timeout_secs", Type: "int", Default: "30", Desc: "download timeout in seconds (control node)"},
		{Name: "headers", Type: "map", Desc: "extra request headers (e.g. Authorization)"},
	}
}

// Example 示例任务。
func (m *ArtifactModule) Example() string {
	return `# one task for both online and offline: cache packages/ pre-seeded -> offline;
# empty cache -> download once on the control node, then distribute
- name: distribute kubernetes components from the server bundle
  artifact:
    url: "https://dl.k8s.io/v1.31.0/kubernetes-server-linux-amd64.tar.gz"
    cache: "packages/{{ .arch }}/kubernetes-server-{{ .version }}.tar.gz"
    members: [kube-apiserver, kube-controller-manager, kube-scheduler, kubelet, kubectl]
    dest: "{{ .bin_dir }}"
    mode: "0755"

# plain binary artifact (no archive): dest is the file path
- name: distribute etcdctl
  artifact:
    url: "https://github.com/etcd-io/etcd/releases/download/{{ .etcd_version }}/etcd-{{ .etcd_version }}-linux-{{ .arch }}.tar.gz"
    cache: "packages/{{ .arch }}/etcd-{{ .etcd_version }}.tar.gz"
    members: [etcdctl]
    dest: "{{ .bin_dir }}"
    mode: "0755"
`
}

// Run 执行制品分发。
func (m *ArtifactModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	cache, ok := argStr(args, "cache")
	if !ok || cache == "" {
		return Fail("%s", "artifact requires a cache parameter")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("%s", "artifact requires a dest parameter")
	}
	url, _ := argStr(args, "url")
	members, _ := argStrList(args, "members")
	wantSum, _ := argStr(args, "sha256")
	wantSum = strings.ToLower(strings.TrimSpace(wantSum))
	if wantSum != "" && !isSHA256Hex(wantSum) {
		return Fail("%s", "sha256 parameter must be a 64-character hex string")
	}
	timeoutSecs, ok := argSecs(args, "timeout_secs", 30)
	if !ok || timeoutSecs <= 0 {
		return Fail("%s", "timeout_secs must be a positive integer")
	}
	headers, bad := headerMapArg(args, "headers")
	if bad != nil {
		return bad
	}
	forcedMode, hasMode := argMode(args, "mode")
	if !hasMode {
		forcedMode = 0o755 // 制品以可执行文件为主，缺省 0755（members 沿用归档权限）
	}

	cachePath := resolveLocal(rc, cache)
	data, cacheHit, res := m.loadCache(rc, cachePath, url, wantSum, headers, timeoutSecs)
	if res != nil {
		return res
	}

	if len(members) > 0 {
		return m.distributeMembers(rc, data, cacheHit, cache, dest, members, int64(forcedMode.Perm()), hasMode)
	}

	changed, res := putFile(rc, data, dest, int64(forcedMode.Perm()), false, true, "", "")
	if res != nil {
		return res
	}
	return &Result{Changed: changed, Msg: m.msg(cacheHit, cache, changed,
		fmt.Sprintf("%s content is unchanged", dest), fmt.Sprintf("distributed %d bytes to %s", len(data), dest))}
}

// loadCache 解析制品来源：cache 命中（含 sha256 校验通过）直接用；未命中
// 或校验失败时经 url 下载并原子落缓存。check 模式不产生控制端写入，只预估。
func (m *ArtifactModule) loadCache(rc *RunContext, cachePath, url, wantSum string, headers map[string]string, timeoutSecs int) ([]byte, bool, *Result) {
	if data, err := os.ReadFile(cachePath); err == nil && len(data) > 0 {
		if wantSum == "" || sha256hex(data) == wantSum {
			return data, true, nil
		}
		// 缓存损坏/版本漂移：有 url 时自愈重下，无 url 则失败（离线包被动过）
		if url == "" {
			return nil, false, Fail("cached artifact %s fails the sha256 check and no url is set to refresh it", cachePath)
		}
	}
	if rc.CheckMode {
		// check 只读：不下载不落缓存（下载落盘是控制端变更）
		return nil, false, &Result{Changed: true, Msg: fmt.Sprintf("[check] cache miss: would download %s into %s and distribute", url, cachePath)}
	}
	if url == "" {
		return nil, false, Fail("offline artifact %s is missing (prepare it with the chart's download phase or ship it inside the package; no url is set to fetch it)", cachePath)
	}
	data, bad := (&GetURLModule{}).fetch(rc, url, headers, timeoutSecs)
	if bad != nil {
		return nil, false, bad
	}
	if wantSum != "" && sha256hex(data) != wantSum {
		return nil, false, Fail("download checksum mismatch for %s (expected %s)", url, wantSum)
	}
	if err := writeCache(cachePath, data); err != nil {
		return nil, false, Fail("failed to write artifact cache %s: %v", cachePath, err)
	}
	return data, false, nil
}

// distributeMembers 从归档制品中选取成员并逐个分发（拍平到 dest/，basename
// 命中；权限沿用归档条目，mode 参数显式给出时强制覆盖）。
func (m *ArtifactModule) distributeMembers(rc *RunContext, data []byte, cacheHit bool, cache, dest string, members []string, forcedMode int64, hasMode bool) *Result {
	kind := archiveKind(cache)
	sel, err := selectArchiveMembers(kind, data, members)
	if err != nil {
		return Fail("artifact %s: %v", cache, err)
	}
	cur, bad := probePath(rc, dest)
	if bad != nil {
		return bad
	}
	if cur != "missing" && cur != "directory" {
		return Fail("%s exists and is not a directory", dest)
	}
	if cur == "missing" {
		if out, bad := rc.exec(fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(dest))); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail("failed to create directory: %s", firstLine(out.Stderr))
		}
	}
	changed := false
	for _, mem := range sel {
		mode := mem.mode
		if mode == 0 {
			mode = 0o644
		}
		if hasMode {
			mode = forcedMode
		}
		target := strings.TrimSuffix(dest, "/") + "/" + filepath.ToSlash(filepath.Base(strings.ReplaceAll(mem.name, `\`, "/")))
		memChanged, res := putFile(rc, mem.data, target, mode, false, true, "", "")
		if res != nil {
			if res.Failed {
				return res
			}
			// check 预估：逐成员累积 changed，继续评估其余成员
			if res.Changed {
				changed = true
			}
			continue
		}
		if memChanged {
			changed = true
		}
	}
	return &Result{Changed: changed, Msg: m.msg(cacheHit, cache, changed,
		fmt.Sprintf("%d members from %s are unchanged in %s", len(sel), cache, dest),
		fmt.Sprintf("distributed %d members from %s to %s", len(sel), cache, dest))}
}

// writeCache 原子落缓存（临时名 + rename，并发执行不会留半成品）。
func writeCache(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp-" + tempSuffix()
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
