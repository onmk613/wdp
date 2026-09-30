package module

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&GetURLModule{})
}

// GetURLModule 从 URL 下载文件到远端。两种模式：
//
//	via: remote（缺省）——下载在目标机执行（curl/wget）：大文件不占
//	  控制端带宽与内存，也不受 agent 请求体上限约束；sha256 在目标机
//	  本地校验，幂等语义与 local 一致。
//	via: local——控制端拉取（校验 sha256）后经 putFile 分发，复用其
//	  幂等/备份/回滚/check/diff 语义。目标机不出网、或需要控制端代理
//	  （headers 带凭据不落到目标机进程列表）时用。
//	（键名刻意避开 mode：mode 是文件权限，存量任务在用）
type GetURLModule struct{}

func (m *GetURLModule) Name() string { return "get_url" }

func (m *GetURLModule) Desc() string {
	return i18n.T("Download a URL onto the remote host (the target host fetches directly by default; via:local relays through the controller)", "下载 URL 到远端（默认目标机直连下载；via:local 经控制端中转）")
}

func (m *GetURLModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "url", Type: "string", Desc: i18n.T("download URL (http/https)", "下载地址（http/https）")},
		{Name: "dest", Type: "string", Desc: i18n.T("remote destination path", "远端目标路径")},
		{Name: "via", Type: "string", Default: "remote", Enum: []string{"remote", "local"},
			Desc: i18n.T("download route: direct from the target host (curl/wget) or relayed through the controller (for target hosts with no outbound network or using the controller as a proxy)", "下载路径：目标机直连（curl/wget）或经控制端中转（目标机不出网/走控制端代理时用）")},
		{Name: "sha256", Type: "string", Desc: i18n.T("expected sha256 (64 hex digits): verifies the downloaded content; skipped when the remote file already matches", "期望 sha256（64 位十六进制）：校验下载内容；远端文件已匹配时直接跳过")},
		{Name: "mode", Type: "mode", Default: "0644", Desc: i18n.T("mode of the target file", "目标文件权限位")},
		{Name: "owner", Type: "string", Desc: i18n.T("owner (requires become)", "属主（需 become）")},
		{Name: "group", Type: "string", Desc: i18n.T("group (requires become)", "属组（需 become）")},
		{Name: "backup", Type: "bool", Default: "false", Desc: i18n.T("back up before overwriting (dest.bak.<timestamp>)", "覆盖前先备份（dest.bak.<时间戳>）")},
		{Name: "timeout_secs", Type: "int", Default: "30", Desc: i18n.T("download timeout in seconds", "下载超时（秒）")},
		{Name: "headers", Type: "map", Desc: i18n.T("extra request headers (e.g. Authorization)", "附加请求头（如 Authorization）")},
	}
}

func (m *GetURLModule) Example() string {
	return i18n.T(`- name: Download directly on the target host (default; no controller bandwidth)
  get_url:
    url: https://example.com/releases/app/v1.2.0/app-linux-amd64
    dest: /usr/local/bin/app
    mode: "0755"
    sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08

- name: No outbound network on the target host: download and verify via the controller, then distribute
  get_url:
    url: https://mirror.internal/app.tgz
    dest: /opt/app.tgz
    via: local
    headers:
      Authorization: "Bearer {{ .download_token }}"
`, `- name: 目标机直接下载（缺省；不占控制端带宽）
  get_url:
    url: https://example.com/releases/app/v1.2.0/app-linux-amd64
    dest: /usr/local/bin/app
    mode: "0755"
    sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08

- name: 目标机不出网：经控制端下载校验后分发
  get_url:
    url: https://mirror.internal/app.tgz
    dest: /opt/app.tgz
    via: local
    headers:
      Authorization: "Bearer {{ .download_token }}"
`)
}

// Run 执行下载分发。
func (m *GetURLModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	url, ok := argStr(args, "url")
	if !ok || url == "" {
		return Fail("get_url requires a url parameter")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("get_url requires a dest parameter")
	}
	wantSum, _ := argStr(args, "sha256")
	wantSum = strings.ToLower(strings.TrimSpace(wantSum))
	if wantSum != "" && !isSHA256Hex(wantSum) {
		return Fail("sha256 parameter must be a 64-character hex string")
	}
	via, _ := argStr(args, "via")
	dlMode := strings.ToLower(strings.TrimSpace(via)) // 空值缺省 remote
	if dlMode == "" {
		dlMode = "remote"
	}
	if dlMode != "remote" && dlMode != "local" {
		return Fail("via parameter must be remote or local, got %q", dlMode)
	}

	mode := fs.FileMode(0o644) // 缺省 0644（始终显式下发，覆盖上传缺省）
	if mv, ok := argMode(args, "mode"); ok {
		mode = mv.Perm()
	}
	owner, _ := argStr(args, "owner")
	group, _ := argStr(args, "group")
	backup, _ := argBool(args, "backup")
	timeoutSecs, ok := argSecs(args, "timeout_secs", 30)
	if !ok || timeoutSecs <= 0 {
		return Fail("timeout_secs must be a positive integer")
	}
	headers, bad := headerMapArg(args, "headers")
	if bad != nil {
		return bad
	}

	// 幂等短路（两种模式共用）：期望校验和已给出且远端一致时免下载，
	// 仅校正权限/属主
	if wantSum != "" {
		remote, exists, bad := remoteChecksum(rc, dest)
		if bad != nil {
			return bad
		}
		if exists && remote == wantSum {
			return m.skipDownload(rc, dest, mode, owner, group)
		}
	}

	if dlMode == "local" {
		return m.runLocal(rc, url, dest, wantSum, mode, owner, group, backup, timeoutSecs, headers)
	}
	return m.runRemote(rc, url, dest, wantSum, mode, owner, group, backup, timeoutSecs, headers)
}

// runLocal 控制端下载 + putFile 分发（原实现，语义不变）。
func (m *GetURLModule) runLocal(rc *RunContext, url, dest, wantSum string, mode fs.FileMode, owner, group string, backup bool, timeoutSecs int, headers map[string]string) *Result {
	data, bad := m.fetch(rc, url, headers, timeoutSecs)
	if bad != nil {
		return bad
	}
	if wantSum != "" {
		if got := sha256hex(data); got != wantSum {
			return Fail("download checksum mismatch: sha256 expected %s got %s (url=%s)", wantSum, got, url)
		}
	}
	changed, res := putFile(rc, putFileOpts{data: data, dest: dest, mode: &mode, backup: backup, owner: owner, group: group})
	if res != nil {
		return res // 失败或 check 预估（含 --diff 内容差异）直接透传
	}
	msg := fmt.Sprintf("%s content is unchanged", dest)
	if changed {
		msg = fmt.Sprintf("downloaded %s to %s", url, dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// runRemote 目标机直接下载：目标机执行 curl（缺 wget 兜底）落到同目录
// 临时文件，sha256 在目标机校验通过后原子 mv 到 dest。退出码约定：
// 42=校验和不匹配（已清理临时文件），其余非零=下载器失败。
func (m *GetURLModule) runRemote(rc *RunContext, url, dest, wantSum string, mode fs.FileMode, owner, group string, backup bool, timeoutSecs int, headers map[string]string) *Result {
	if bad := requireBecomeForOwner(rc, owner, group, dest); bad != nil {
		return bad
	}
	if rc.CheckMode {
		return &Result{Changed: true, Msg: fmt.Sprintf("[check] would download %s to %s (remote mode)", url, dest)}
	}

	// 变更判定：下载前记下已有内容校验和（多一次探测，仅 dest 已存在时），
	// 下载后脚本回显新校验和，一致则 changed=false（与 local 模式口径对齐）
	preSum, preExists, bad := remoteChecksum(rc, dest)
	if bad != nil {
		return bad
	}

	// 上限与 local 模式同源：注入值优先，否则内置 2GiB（curl --max-filesize
	// 需服务端给 Content-Length 才强制，chunked 时尽力而为）
	limit := rc.MaxDownloadBytes
	if limit <= 0 {
		limit = defaultTransferLimit
	}
	out, xbad := rc.exec(m.remoteScript(url, dest, wantSum, mode, backup, timeoutSecs, headers, limit))
	if xbad != nil {
		return xbad
	}
	if out.Code == 42 {
		return Fail("download checksum mismatch on remote host (url=%s)", url)
	}
	if out.Code != 0 {
		msg := firstLine(out.Stderr)
		if msg == "" {
			msg = firstLine(out.Stdout)
		}
		return Fail("remote download failed rc=%d: %s (url=%s)", out.Code, msg, url)
	}
	newSum := ""
	for line := range strings.SplitSeq(out.Stdout, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "wdp_sum="); ok {
			newSum = strings.TrimSpace(v)
		}
	}
	// 内容未变判定：dest 原已存在、下载后校验和一致 → 与 local 模式口径对齐
	changed := !preExists || newSum == "" || preSum != newSum
	// 属主/属组：下载路径只落内容与权限，属主漂移交给 fixAttrs（需 become）
	if owner != "" || group != "" {
		if _, _, fbad := fixAttrs(rc, dest, nil, owner, group); fbad != nil {
			return fbad
		}
	}
	msg := fmt.Sprintf("downloaded %s to %s (remote)", url, dest)
	if !changed {
		msg = fmt.Sprintf("%s content is unchanged", dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// remoteScript 目标机下载脚本。所有插值（URL/headers/路径/校验和）经
// shellquote 单引号字面量化，防 K=V/命令替换注入。
func (m *GetURLModule) remoteScript(url, dest, wantSum string, mode fs.FileMode, backup bool, timeoutSecs int, headers map[string]string, maxBytes int64) string {
	var b strings.Builder
	q := shellquote.Quote
	fmt.Fprintf(&b, "tmp=$(mktemp %s.wdp.XXXXXX) || exit 1\n", q(dest+"."))
	if backup {
		fmt.Fprintf(&b, "[ ! -f %s ] || cp -p %s %s.bak.$(date +%%s)\n", q(dest), q(dest), q(dest))
	}
	b.WriteString("if command -v curl >/dev/null 2>&1; then\n  curl -fsSL")
	fmt.Fprintf(&b, " --connect-timeout 10 --max-time %d", timeoutSecs)
	if maxBytes > 0 {
		fmt.Fprintf(&b, " --max-filesize %d", maxBytes)
	}
	for k, v := range headers {
		fmt.Fprintf(&b, " -H %s", q(k+": "+v))
	}
	fmt.Fprintf(&b, " -o \"$tmp\" %s\nelse\n  wget -q --timeout=%d", q(url), timeoutSecs)
	for k, v := range headers {
		fmt.Fprintf(&b, " --header=%s", q(k+": "+v))
	}
	fmt.Fprintf(&b, " -O \"$tmp\" %s\nfi\n[ -s \"$tmp\" ] || { rm -f \"$tmp\"; echo 'download failed or empty file' >&2; exit 1; }\n", q(url))
	if wantSum != "" {
		b.WriteString("if command -v sha256sum >/dev/null 2>&1; then sum=$(sha256sum \"$tmp\" | awk '{print $1}'); else sum=$(shasum -a 256 \"$tmp\" | awk '{print $1}'); fi\n")
		fmt.Fprintf(&b, "[ \"$sum\" = %s ] || { rm -f \"$tmp\"; echo 'checksum mismatch' >&2; exit 42; }\n", q(wantSum))
	}
	fmt.Fprintf(&b, "chmod %#o \"$tmp\" && mv -f \"$tmp\" %s\n", mode.Perm(), q(dest))
	b.WriteString("sum=\"\"\nif command -v sha256sum >/dev/null 2>&1; then sum=$(sha256sum \"$dest\" 2>/dev/null | awk '{print $1}'); else sum=$(shasum -a 256 \"$dest\" 2>/dev/null | awk '{print $1}'); fi\n")
	b.WriteString("echo \"wdp_sum=$sum\"\n")
	b.WriteString("rm -f \"$tmp\" 2>/dev/null\nexit 0\n")
	return b.String()
}

// skipDownload 处理远端校验和已一致时的收尾：check 模式仅预估属性变更，
// 实模式校正权限/属主（内容不动，无备份与回滚登记需求，走 fixAttrs）。
func (m *GetURLModule) skipDownload(rc *RunContext, dest string, mode fs.FileMode, owner, group string) *Result {
	if bad := requireBecomeForOwner(rc, owner, group, dest); bad != nil {
		return bad
	}
	if rc.CheckMode {
		would := false
		if owner != "" || group != "" {
			drift, obad := ownerGroupDrift(rc, dest, owner, group)
			if obad != nil {
				return obad
			}
			would = drift
		}
		if cur, ok, mbad := remoteMode(rc, dest); mbad != nil {
			return mbad
		} else if ok && cur != int64(mode.Perm()) {
			would = true
		}
		return &Result{Changed: would, Msg: fmt.Sprintf("[check] %s content is unchanged (sha256 matches)", dest)}
	}
	fixedMode, fixedOwner, bad := fixAttrs(rc, dest, &mode, owner, group)
	if bad != nil {
		return bad
	}
	changed := fixedMode || fixedOwner
	msg := fmt.Sprintf("%s content is unchanged (sha256 matches, download skipped)", dest)
	if changed {
		msg = fmt.Sprintf("%s attributes corrected (content unchanged)", dest)
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
		return nil, Fail("unable to parse URL: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, Fail("download failed %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, Fail("download failed %s: HTTP %d", url, resp.StatusCode)
	}
	// 响应体上限：RunContext 注入值优先（CLI/配置文件），否则内置缺省
	// 2GiB（defaultTransferLimit：无上限时异常/恶意 URL 的下载可在超时
	// 窗口内累积数 GB 内存导致控制端 OOM）
	limit := rc.MaxDownloadBytes
	if limit <= 0 {
		limit = defaultTransferLimit
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, perr := strconv.ParseInt(cl, 10, 64); perr == nil && n > limit {
			return nil, Fail("download failed %s: response body %d bytes exceeds the %d limit", url, n, limit)
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, Fail("failed to read response body %s: %v", url, err)
	}
	if len(data) > int(limit) {
		return nil, Fail("download failed %s: response body exceeds the %d byte limit (suspected abnormal/malicious URL)", url, limit)
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
		return nil, Fail("%s parameter must be a key-value mapping", key)
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
