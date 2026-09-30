package module

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"wdp/internal/conn"
	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&UnarchiveModule{})
}

// UnarchiveModule 将本地（或远端）tar/zip 归档解压到远端目录。
// 控制端无法预知归档内容，幂等性由 creates 守卫提供（见 Example）。
type UnarchiveModule struct{}

func (m *UnarchiveModule) Name() string { return "unarchive" }

// RollbackCapability 部分可回滚：回滚日志仅登记"删除本次新建目录"
// （RecordRemove），覆盖已有目录内的文件不恢复快照。
func (m *UnarchiveModule) RollbackCapability() RollbackCapability { return RollbackPartial }

func (m *UnarchiveModule) Desc() string {
	return i18n.T("Extract tar/zip into a remote directory (local distribution or in-place extraction on the remote)", "解包 tar/zip 到远端目录（本地分发或远端原地解包）")
}

func (m *UnarchiveModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: i18n.T("local archive path (relative to the playbook, resolved against BaseDir)", "本地压缩包路径（playbook 内相对路径，按 BaseDir 解析）")},
		{Name: "dest", Type: "string", Desc: i18n.T("remote destination directory (created if missing)", "远端目标目录（不存在则创建）")},
		{Name: "remote_src", Type: "bool", Default: "false", Desc: i18n.T("src is a path on the target host (skips the upload and extracts in place; pairs with get_url's default direct download)", "src 是目标机上的路径（跳过上传，原地解包；与 get_url 缺省直连下载配套）")},
		{Name: "creates", Type: "string", Desc: i18n.T("idempotency guard: skip the task if this path exists (the controller cannot see inside the archive, so repeated runs rely on it to converge)", "幂等守卫：该路径存在则跳过任务（控制端看不到压缩包内容，重复执行靠它收敛）")},
		{Name: "members", Type: "list", Desc: i18n.T("extract only these entries (matched by basename or full in-archive path, flattened into dest/; works with local or remote src; fails if no name matches)", "只解包这些条目（按 basename 或完整包内路径匹配，拍平进 dest/；本地/远端 src 均可；未匹配到名字则失败）")},
	}
}

func (m *UnarchiveModule) Example() string {
	return i18n.T(`# distribute and extract (the creates guard keeps it idempotent: repeat runs skip outright)
- name: deploy the application package
  unarchive:
    src: files/myapp-1.2.3.tar.gz   # local archive, relative to the playbook directory
    dest: /opt/myapp
    creates: /opt/myapp/bin/myapp    # skip if the file already exists, avoiding a redundant extract-over

# archive already on the remote, extract only
- name: extract the remote archive
  unarchive:
    src: /tmp/data.zip
    dest: /srv/data
    remote_src: true
    creates: /srv/data/README

# target host downloads it (get_url direct) + extract selected members in place:
# the classic docker playbook move -- flatten the needed binaries into bin_dir
- name: let the target host download it
  get_url: {url: "https://.../docker-{{ .docker_version }}.tgz", dest: /tmp/docker.tgz, sha256: "..."}
- name: extract selected members from the remote archive
  unarchive:
    src: /tmp/docker.tgz
    dest: /usr/bin
    remote_src: true
    members: [dockerd, docker, containerd, runc]`, `# 分发并解包（creates 守卫保证幂等：重复执行直接跳过）
- name: 部署应用包
  unarchive:
    src: files/myapp-1.2.3.tar.gz   # 本地压缩包，相对 playbook 目录
    dest: /opt/myapp
    creates: /opt/myapp/bin/myapp    # 文件已存在即跳过，避免重复解包覆盖

# 压缩包已在远端，只解包
- name: 解包远端压缩包
  unarchive:
    src: /tmp/data.zip
    dest: /srv/data
    remote_src: true
    creates: /srv/data/README

# 目标机下载（get_url 直连）+ 就地解包指定成员：
# docker playbook 的经典套路——把要的二进制拍平进 bin_dir
- name: 目标机自行下载
  get_url: {url: "https://.../docker-{{ .docker_version }}.tgz", dest: /tmp/docker.tgz, sha256: "..."}
- name: 从远端压缩包解包指定成员
  unarchive:
    src: /tmp/docker.tgz
    dest: /usr/bin
    remote_src: true
    members: [dockerd, docker, containerd, runc]`)
}

// unarchiveReq 是 unarchive 解析后的参数。
type unarchiveReq struct {
	src        string
	dest       string
	remoteSrc  bool
	creates    string
	members    []string
	hasMembers bool
	kind       string // zip / targz / tarxz / tar
}

// parseUnarchiveArgs 解析并校验 unarchive 参数（src/dest 必填、归档
// 扩展名识别）。
func parseUnarchiveArgs(rc *RunContext, args map[string]any) (*unarchiveReq, *Result) {
	src, ok := argStr(args, "src")
	if !ok || src == "" {
		return nil, Fail("unarchive requires a src parameter")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return nil, Fail("unarchive requires a dest parameter")
	}
	r := &unarchiveReq{src: src, dest: dest}
	r.remoteSrc, _ = argBool(args, "remote_src")
	r.creates, _ = argStr(args, "creates")
	r.members, r.hasMembers = argStrList(args, "members")
	r.kind = archiveKind(src)
	if r.kind == "" {
		return nil, Fail("unrecognized archive format %q (supported: .tar/.tgz/.tar.gz/.tar.xz/.txz/.zip)", src)
	}
	return r, nil
}

// remotePathExists 探测远端路径是否存在（creates 守卫与 remote_src 的
// 存在性检查共用）。
func remotePathExists(rc *RunContext, path string) (bool, *Result) {
	out, bad := rc.exec(fmt.Sprintf("[ -e %s ]", shellquote.Quote(path)))
	if bad != nil {
		return false, bad
	}
	return out.Code == 0, nil
}

// Run 执行解压：解析 → creates 幂等守卫 → members 分发或整包解压
// （骨架与 user 模块一致：parse → probe/check → apply）。
func (m *UnarchiveModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	r, bad := parseUnarchiveArgs(rc, args)
	if bad != nil {
		return bad
	}

	// creates 守卫：目标标记已存在则跳过（幂等）
	if r.creates != "" {
		exists, bad := remotePathExists(rc, r.creates)
		if bad != nil {
			return bad
		}
		if exists {
			return &Result{Msg: fmt.Sprintf("%s already exists, skipped", r.creates)}
		}
	}

	// members 选取路径：控制端按名检索归档成员，逐文件分发（拍平到 dest/，
	// 校验和幂等，权限沿用归档条目）——不再整体解压
	if r.hasMembers && len(r.members) > 0 {
		if r.remoteSrc {
			return m.runMembersRemote(rc, r.src, r.kind, r.dest, r.members)
		}
		return m.runMembers(rc, r.src, r.kind, r.dest, r.members)
	}

	// 控制端无法预知归档内容，check 模式只报告将执行解压
	if rc.CheckMode {
		return unarchiveCheck(rc, r)
	}
	return m.runWholeArchive(rc, r)
}

// unarchiveCheck 是整包解压的 check 预估：只报告将执行解压，--diff 下
// 区分目标目录"新建"与"覆盖"两种情形。
func unarchiveCheck(rc *RunContext, r *unarchiveReq) *Result {
	res := &Result{Changed: true, Msg: fmt.Sprintf("[check] would extract %s to %s", r.src, r.dest)}
	if rc.DiffMode {
		cur, bad := probePath(rc, r.dest)
		if bad != nil {
			return bad
		}
		var d []string
		if cur == "missing" {
			d = append(d, fmt.Sprintf("+ %s (create directory and extract %s)", r.dest, r.src))
		} else {
			d = append(d, fmt.Sprintf("- %s (%s)", r.dest, cur),
				fmt.Sprintf("+ %s (extract %s overwrites, archive content is invisible to the controller)", r.dest, r.src))
		}
		res.Diff = strings.Join(d, "\n")
	}
	return res
}

// runWholeArchive 实跑整包解压：remote_src 存在性检查 → 目标目录预检 →
// 本地归档上传临时路径（结束自删）→ 原生解压优先、shell 命令兜底。
func (m *UnarchiveModule) runWholeArchive(rc *RunContext, r *unarchiveReq) *Result {
	// remote_src：归档必须在远端存在
	if r.remoteSrc {
		exists, bad := remotePathExists(rc, r.src)
		if bad != nil {
			return bad
		}
		if !exists {
			return Fail("remote archive not found: %s", r.src)
		}
	}

	// 目标目录存在性探测：新建目录登记回滚删除
	cur, bad := destDirState(rc, r.dest)
	if bad != nil {
		return bad
	}

	// 本地 src：读取并上传到远端临时路径。上传的临时副本是本模块的
	// 实现细节，成败路径都清理（此前默认与失败路径会在远端 /tmp 残留）
	remoteArc := r.src
	if !r.remoteSrc {
		local, lerr := resolveLocal(rc, r.src)
		if lerr != nil {
			return Fail("%v", lerr)
		}
		data, err := readLocalSrc(rc, local)
		if err != nil {
			return Fail("failed to read local archive: %v", err)
		}
		remoteArc = "/tmp/.wdp-arc-" + tempSuffix()
		if err := uploadBytes(rc, remoteArc, data, 0o600, true); err != nil {
			return Fail("failed to upload archive: %v", err)
		}
		defer func() { // best-effort：ctx 已取消时失败可接受（下轮重跑会换新临时名）
			_, _ = rc.exec(fmt.Sprintf("rm -f -- %s", shellquote.Quote(remoteArc)))
		}()
	}

	if rc.Rollback != nil && cur == "missing" {
		rc.Rollback.RecordRemove(r.dest)
	}

	// 解压双路径：agent/push 通道优先原生（agent 侧 Go 实现，不依赖目标机
	// tar/unzip/xz，端点自建目标目录）；旧版 agent（404 哨兵）与 SSH 通道
	// 回退 shell 命令
	if nx, ok := rc.Conn.(conn.NativeExtractor); ok {
		if err := nx.NativeExtract(rc.Ctx, remoteArc, r.dest); err == nil {
			return &Result{Changed: true, Msg: fmt.Sprintf("extracted %s to %s (native)", r.src, r.dest)}
		} else if !errors.Is(err, conn.ErrNativeUnsupported) {
			return Fail("extract failed: %v", err)
		}
	}

	// zip 依赖 unzip，提前给出可读错误（原生路径无此依赖）
	if r.kind == "zip" {
		out, bad := rc.exec("command -v unzip >/dev/null 2>&1")
		if bad != nil {
			return bad
		}
		if out.Code != 0 {
			return Fail("target machine is missing unzip (installing the unzip package is required to extract .zip)")
		}
	}

	if out, bad := rc.exec(fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(r.dest))); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("failed to create directory: %s", firstLine(out.Stderr))
	}

	// 解压命令：tar 系列统一 -C dest；zip 用 unzip -o 覆盖解压
	script := fmt.Sprintf("tar -x%sf %s -C %s", tarFlag(r.kind), shellquote.Quote(remoteArc), shellquote.Quote(r.dest))
	if r.kind == "zip" {
		script = fmt.Sprintf("unzip -o %s -d %s", shellquote.Quote(remoteArc), shellquote.Quote(r.dest))
	}
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("extract failed: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("extracted %s to %s", r.src, r.dest)}
}

// runMembers 执行成员选取分发：本地归档按名检索成员（basename 或完整路径，
// 未命中报错），逐个拍平写入 dest/，复用 putFile 的校验和幂等与回滚登记。
func (m *UnarchiveModule) runMembers(rc *RunContext, src, kind, dest string, members []string) *Result {
	local, lerr := resolveLocal(rc, src)
	if lerr != nil {
		return Fail("%v", lerr)
	}
	data, err := readLocalSrc(rc, local)
	if err != nil {
		return Fail("failed to read local archive: %v", err)
	}
	sel, err := selectArchiveMembers(kind, data, members, rc.MaxUploadBytes)
	if err != nil {
		return Fail("unarchive %s: %v", src, err)
	}

	cur, bad := destDirState(rc, dest)
	if bad != nil {
		return bad
	}
	// 新建目录登记回滚删除（与整包解压路径同口径：此前 members 路径漏
	// 登记，auto_rollback 后残留空目录，与 RollbackPartial 声明不符）
	recordMkdirRollback(rc, dest, cur)
	if bad := mkdirDestMissing(rc, dest, cur); bad != nil {
		return bad
	}

	changed, bad := distributeMemberFiles(rc, dest, sel, 0, false)
	if bad != nil {
		return bad
	}
	if rc.CheckMode {
		return &Result{Changed: changed, Msg: fmt.Sprintf("[check] would extract %d member(s) from %s to %s", len(sel), src, dest)}
	}
	msg := fmt.Sprintf("%d member(s) from %s are unchanged in %s", len(sel), src, dest)
	if changed {
		msg = fmt.Sprintf("extracted %d member(s) from %s to %s", len(sel), src, dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// runMembersRemote 目标机归档的成员选取：远端暂存目录整包解压一次，
// 按 basename 检索成员拍平到 dest/（cp -p 保留归档条目权限），cmp 比对
// 实现幂等，未命中成员报 42。与本地 members 语义一致（拍平、未命中即
// 失败），配 get_url via:remote 形成"目标机下载 → 目标机解压"的完整链路。
func (m *UnarchiveModule) runMembersRemote(rc *RunContext, src, kind, dest string, members []string) *Result {
	// check 模式无法预知归档内容：只预估（与整包解压同口径）
	if rc.CheckMode {
		return &Result{Changed: true, Msg: fmt.Sprintf("[check] would extract %d member(s) from %s to %s (remote src)", len(members), src, dest)}
	}
	exists, bad := remotePathExists(rc, src)
	if bad != nil {
		return bad
	}
	if !exists {
		return Fail("remote archive not found: %s", src)
	}
	cur, bad := destDirState(rc, dest)
	if bad != nil {
		return bad
	}
	recordMkdirRollback(rc, dest, cur)
	if bad := mkdirDestMissing(rc, dest, cur); bad != nil {
		return bad
	}

	// 暂存解压 + 逐成员检索拍平。单次解压（逐成员 tar -xO 会反复解压
	// 80MB 级归档 N 遍）；所有插值经 shellquote 字面量化
	q := shellquote.Quote
	var b strings.Builder
	b.WriteString("stage=$(mktemp -d \"${TMPDIR:-/tmp}/wdp-arc.XXXXXX\") || exit 1\n")
	if kind == "zip" {
		fmt.Fprintf(&b, "unzip -qq %s -d \"$stage\" 2>/dev/null", q(src))
	} else {
		fmt.Fprintf(&b, "tar -x%sf %s -C \"$stage\" 2>/dev/null", tarFlag(kind), q(src))
	}
	b.WriteString(" || { rm -rf \"$stage\"; echo 'extract to stage failed' >&2; exit 1; }\n")
	b.WriteString("missing=0; changed=0\n")
	for _, mem := range members {
		fmt.Fprintf(&b, "f=$(find \"$stage\" -type f -name %s | head -n 1)\n", q(mem))
		fmt.Fprintf(&b, "if [ -n \"$f\" ]; then\n")
		fmt.Fprintf(&b, "  if [ -f %s ] && cmp -s \"$f\" %s; then :; else cp -p \"$f\" %s; changed=1; fi\n", q(dest+"/"+mem), q(dest+"/"+mem), q(dest+"/"+mem))
		fmt.Fprintf(&b, "else echo \"member not found: %s\" >&2; missing=1; fi\n", mem)
	}
	b.WriteString("rm -rf \"$stage\"\n")
	b.WriteString("echo \"wdp_missing=$missing wdp_changed=$changed\"\n")
	out, xbad := rc.exec(b.String())
	if xbad != nil {
		return xbad
	}
	if out.Code != 0 {
		return Fail("remote member extract failed rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	if strings.Contains(out.Stdout, "wdp_missing=1") {
		return Fail("member(s) not found in remote archive %s: %s (see task stderr)", src, firstLine(out.Stderr))
	}
	changed := strings.Contains(out.Stdout, "wdp_changed=1")
	msg := fmt.Sprintf("%d member(s) from %s are unchanged in %s (remote src)", len(members), src, dest)
	if changed {
		msg = fmt.Sprintf("extracted %d member(s) from %s to %s (remote src)", len(members), src, dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// archiveKind 按扩展名识别归档类型：zip / targz / tarxz / tar（无法识别返回空）。
func archiveKind(src string) string {
	l := strings.ToLower(src)
	switch {
	case strings.HasSuffix(l, ".zip"):
		return "zip"
	case strings.HasSuffix(l, ".tgz"), strings.HasSuffix(l, ".tar.gz"):
		return "targz"
	case strings.HasSuffix(l, ".txz"), strings.HasSuffix(l, ".tar.xz"):
		return "tarxz"
	case strings.HasSuffix(l, ".tar"):
		return "tar"
	}
	return ""
}

// tarFlag 归档类型对应的 tar 解压 flag（gzip / xz / 无压缩）。
func tarFlag(kind string) string {
	switch kind {
	case "targz":
		return "z"
	case "tarxz":
		return "J"
	}
	return ""
}

// sortUnique 返回去重排序后的副本（用户/组列表比较用）。
func sortUnique(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}
