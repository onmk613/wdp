package module

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wdp/internal/i18n"
)

// maxDownloadBytes 控制端单次下载的响应体上限（与 chart 解包 2GiB 上限对齐）。
// 无上限时，指向异常/恶意 URL 的下载可在超时窗口内累积数 GB 内存导致 OOM。
const maxDownloadBytes = 2 << 30

func init() {
	Register(&GetURLModule{})
}

// GetURLModule 从 URL 下载文件到远端：控制端拉取（校验 sha256）后经
// putFile 分发，复用其幂等/备份/回滚/check/diff 语义。
type GetURLModule struct{}

// Name 模块名。
func (m *GetURLModule) Name() string { return "get_url" }

// Desc 模块说明。
func (m *GetURLModule) Desc() string {
	return i18n.T("download a URL to the remote host (sha256 verified, idempotent)", "下载 URL 文件到远端（sha256 校验、幂等）")
}

// Params 参数文档。
func (m *GetURLModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "url", Type: "string", Desc: "下载地址（http/https），控制端发起 GET"},
		{Name: "dest", Type: "string", Desc: "远端目标路径"},
		{Name: "sha256", Type: "string", Desc: "期望 sha256（64 位十六进制）：校验下载内容；远端已一致时跳过下载"},
		{Name: "mode", Type: "mode", Default: "0644", Desc: "目标文件权限"},
		{Name: "owner", Type: "string", Desc: "属主（需 become: true）"},
		{Name: "group", Type: "string", Desc: "属组（需 become: true）"},
		{Name: "backup", Type: "bool", Default: "false", Desc: "覆盖前备份原文件（dest.bak.时间戳）"},
		{Name: "timeout_secs", Type: "int", Default: "30", Desc: "控制端下载超时秒数"},
		{Name: "headers", Type: "map", Desc: "附加请求头（如 Authorization）"},
	}
}

// Example 示例任务。
func (m *GetURLModule) Example() string {
	return `- name: 下载二进制并分发（sha256 校验 + 幂等）
  get_url:
    url: https://example.com/releases/app/v1.2.0/app-linux-amd64
    dest: /usr/local/bin/app
    mode: "0755"
    sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
    headers:
      Authorization: "Bearer {{ .download_token }}"
`
}

// Run 执行下载分发。
func (m *GetURLModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	url, ok := argStr(args, "url")
	if !ok || url == "" {
		return Fail("%s", i18n.T("get_url requires a url parameter", "get_url 需要 url 参数"))
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("%s", i18n.T("get_url requires a dest parameter", "get_url 需要 dest 参数"))
	}
	wantSum, _ := argStr(args, "sha256")
	wantSum = strings.ToLower(strings.TrimSpace(wantSum))
	if wantSum != "" && !isSHA256Hex(wantSum) {
		return Fail("%s", i18n.T("sha256 parameter must be a 64-character hex string", "sha256 参数应为 64 位十六进制串"))
	}

	mode := int64(0o644) // 缺省 0644（始终显式下发，覆盖上传缺省）
	if mv, ok := argMode(args, "mode"); ok {
		mode = int64(mv.Perm())
	}
	owner, _ := argStr(args, "owner")
	group, _ := argStr(args, "group")
	backup, _ := argBool(args, "backup")
	timeoutSecs, ok := argSecs(args, "timeout_secs", 30)
	if !ok || timeoutSecs <= 0 {
		return Fail("%s", i18n.T("timeout_secs must be a positive integer", "timeout_secs 应为正整数"))
	}
	headers, bad := headerMapArg(args, "headers")
	if bad != nil {
		return bad
	}

	// 幂等短路：期望校验和已给出且远端一致时免下载，仅校正权限/属主
	if wantSum != "" {
		remote, exists, bad := remoteChecksum(rc, dest)
		if bad != nil {
			return bad
		}
		if exists && remote == wantSum {
			return m.skipDownload(rc, dest, mode, owner, group)
		}
	}

	data, bad := m.fetch(rc, url, headers, timeoutSecs)
	if bad != nil {
		return bad
	}
	if wantSum != "" {
		if got := sha256hex(data); got != wantSum {
			return Fail("下载内容校验失败: sha256 期望 %s 实际 %s（url=%s）", wantSum, got, url)
		}
	}

	changed, res := putFile(rc, data, dest, mode, backup, true, owner, group)
	if res != nil {
		return res // 失败或 check 预估（含 --diff 内容差异）直接透传
	}
	msg := fmt.Sprintf(i18n.T("%s content is unchanged", "%s 内容一致"), dest)
	if changed {
		msg = fmt.Sprintf(i18n.T("downloaded %s to %s", "已下载 %s 到 %s"), url, dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// skipDownload 处理远端校验和已一致时的收尾：check 模式仅预估属性变更，
// 实模式校正权限/属主（内容不动，无备份与回滚登记需求）。
// 属主漂移与权限漂移同权重估/校正（此前 check 漏报属主、实跑修复不报 changed）。
func (m *GetURLModule) skipDownload(rc *RunContext, dest string, mode int64, owner, group string) *Result {
	if (owner != "" || group != "") && !rc.Become {
		return Fail(i18n.T("setting owner/group requires become: true (%s)", "设置 owner/group 需要 become: true（%s）"), dest)
	}
	ownerDrift := false
	if owner != "" || group != "" {
		co, cg, ok, obad := remoteOwnerGroup(rc, dest)
		if obad != nil {
			return obad
		}
		ownerDrift = !ok || co != owner || cg != group
	}
	if rc.CheckMode {
		would := ownerDrift
		if cur, ok, mbad := remoteMode(rc, dest); mbad != nil {
			return mbad
		} else if ok && cur != mode {
			would = true
		}
		return &Result{Changed: would, Msg: fmt.Sprintf(i18n.T("[check] %s content is unchanged (sha256 matches)", "[check] %s 内容一致（sha256 匹配）"), dest)}
	}
	changed := false
	if fixed, bad := chmodIfDiffers(rc, dest, mode); bad != nil {
		return bad
	} else if fixed {
		changed = true
	}
	if ownerDrift {
		if bad := chownPath(rc, dest, owner, group); bad != nil {
			return bad
		}
		changed = true
	}
	msg := fmt.Sprintf(i18n.T("%s content is unchanged (sha256 matches, download skipped)", "%s 内容一致（sha256 匹配，跳过下载）"), dest)
	if changed {
		msg = fmt.Sprintf(i18n.T("%s attributes corrected (content unchanged)", "%s 属性已校正（内容一致）"), dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// fetch 在控制端发起 GET（尊重任务 ctx、任务超时与 timeout_secs）。
func (m *GetURLModule) fetch(rc *RunContext, url string, headers map[string]string, timeoutSecs int) ([]byte, *Result) {
	ctx := rc.Ctx
	if rc.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(rc.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, Fail(i18n.T("unable to parse URL: %v", "URL 无法解析: %v"), err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, Fail(i18n.T("download failed %s: %v", "下载失败 %s: %v"), url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, Fail(i18n.T("download failed %s: HTTP %d", "下载失败 %s: HTTP %d"), url, resp.StatusCode)
	}
	// 响应体上限：RunContext 注入值优先（CLI/配置文件），否则内置默认 2GiB
	limit := rc.MaxDownloadBytes
	if limit <= 0 {
		limit = maxDownloadBytes
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, perr := strconv.ParseInt(cl, 10, 64); perr == nil && n > limit {
			return nil, Fail(i18n.T("download failed %s: response body %d bytes exceeds the %d limit", "下载失败 %s: 响应体 %d 字节超过上限 %d"), url, n, limit)
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, Fail(i18n.T("failed to read response body %s: %v", "读取响应体失败 %s: %v"), url, err)
	}
	if len(data) > int(limit) {
		return nil, Fail(i18n.T("download failed %s: response body exceeds the %d byte limit (suspected abnormal/malicious URL)", "下载失败 %s: 响应体超过 %d 字节上限（疑似异常/恶意 URL）"), url, limit)
	}
	return data, nil
}

// headerMapArg 解析 headers 参数（map 形式，值转字符串）。
func headerMapArg(args map[string]any, key string) (map[string]string, *Result) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	switch x := v.(type) {
	case map[string]string:
		return x, nil
	case map[string]any:
		out := make(map[string]string, len(x))
		for k, val := range x {
			out[k] = fmt.Sprint(val)
		}
		return out, nil
	default:
		return nil, Fail(i18n.T("%s parameter must be a key-value mapping", "%s 参数应为键值映射"), key)
	}
}

// isSHA256Hex 判断是否为 64 位十六进制串。
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
