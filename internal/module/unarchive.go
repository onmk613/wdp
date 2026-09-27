package module

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"wdp/internal/conn"
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
	return "extract tar/zip archives into a remote directory"
}

func (m *UnarchiveModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "local archive path (playbook-relative, resolved against BaseDir)"},
		{Name: "dest", Type: "string", Desc: "remote destination directory (created when missing)"},
		{Name: "remote_src", Type: "bool", Default: "false", Desc: "src is a remote path (skip the upload, extract in place)"},
		{Name: "creates", Type: "string", Desc: "idempotency guard: skip the task when this path exists (the control node cannot see archive contents, repeats rely on this)"},
		{Name: "members", Type: "list", Desc: "extract only these entries, matched by basename or full archive path and flattened into dest/ (local src only; unmatched names fail)"},
	}
}

func (m *UnarchiveModule) Example() string {
	return `# distribute and extract (the creates guard makes it idempotent: repeats just skip)
- name: deploy the app package
  unarchive:
    src: files/myapp-1.2.3.tar.gz   # local archive, relative to the playbook dir
    dest: /opt/myapp
    creates: /opt/myapp/bin/myapp    # skip when the file exists, avoiding re-extraction overwrites

# the archive already exists remotely, just extract
- name: extract a remote archive
  unarchive:
    src: /tmp/data.zip
    dest: /srv/data
    remote_src: true
    creates: /srv/data/README`
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
		return m.runMembers(rc, r.src, r.kind, r.dest, r.members, r.remoteSrc)
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
func (m *UnarchiveModule) runMembers(rc *RunContext, src, kind, dest string, members []string, remoteSrc bool) *Result {
	if remoteSrc {
		return Fail("members requires a local src (the control node must inspect the archive to match entries)")
	}
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
