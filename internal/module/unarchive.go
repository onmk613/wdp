package module

import (
	"errors"
	"fmt"
	"os"
	"path"
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

// Name 模块名。
func (m *UnarchiveModule) Name() string { return "unarchive" }

// RollbackCapability 部分可回滚：回滚日志仅登记"删除本次新建目录"
// （RecordRemove），覆盖已有目录内的文件不恢复快照。
func (m *UnarchiveModule) RollbackCapability() RollbackCapability { return RollbackPartial }

// Desc 模块说明。
func (m *UnarchiveModule) Desc() string {
	return "extract tar/zip archives into a remote directory"
}

// Params 参数文档。
func (m *UnarchiveModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "local archive path (playbook-relative, resolved against BaseDir)"},
		{Name: "dest", Type: "string", Desc: "remote destination directory (created when missing)"},
		{Name: "remote_src", Type: "bool", Default: "false", Desc: "src is a remote path (skip the upload, extract in place)"},
		{Name: "creates", Type: "string", Desc: "idempotency guard: skip the task when this path exists (the control node cannot see archive contents, repeats rely on this)"},
		{Name: "members", Type: "list", Desc: "extract only these entries, matched by basename or full archive path and flattened into dest/ (local src only; unmatched names fail)"},
		{Name: "remove", Type: "bool", Default: "false", Desc: "deprecated (kept for compatibility): the uploaded temp archive copy is now always cleaned up automatically"},
	}
}

// Example 示例任务。
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

// Run 执行解压。
func (m *UnarchiveModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	src, ok := argStr(args, "src")
	if !ok || src == "" {
		return Fail("%s", "unarchive requires a src parameter")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("%s", "unarchive requires a dest parameter")
	}
	remoteSrc, _ := argBool(args, "remote_src")
	creates, _ := argStr(args, "creates")
	members, hasMembers := argStrList(args, "members")
	remove, _ := argBool(args, "remove")
	if remove && remoteSrc {
		return Fail("%s", "remove only supports a local src (a remote archive is not cleaned up when remote_src is set)")
	}

	kind := archiveKind(src)
	if kind == "" {
		return Fail("unrecognized archive format %q (supported: .tar/.tgz/.tar.gz/.tar.xz/.txz/.zip)", src)
	}

	// creates 守卫：目标标记已存在则跳过（幂等）
	if creates != "" {
		out, bad := rc.exec(fmt.Sprintf("[ -e %s ]", shellquote.Quote(creates)))
		if bad != nil {
			return bad
		}
		if out.Code == 0 {
			return &Result{Msg: fmt.Sprintf("%s already exists, skipped", creates)}
		}
	}

	// members 选取路径：控制端按名检索归档成员，逐文件分发（拍平到 dest/，
	// 校验和幂等，权限沿用归档条目）——不再整体解压
	if hasMembers && len(members) > 0 {
		return m.runMembers(rc, src, kind, dest, members, remoteSrc)
	}

	// 控制端无法预知归档内容，check 模式只报告将执行解压
	if rc.CheckMode {
		res := &Result{Changed: true, Msg: fmt.Sprintf("[check] would extract %s to %s", src, dest)}
		if rc.DiffMode {
			cur, bad := probePath(rc, dest)
			if bad != nil {
				return bad
			}
			var d []string
			if cur == "missing" {
				d = append(d, fmt.Sprintf("+ %s (create directory and extract %s)", dest, src))
			} else {
				d = append(d, fmt.Sprintf("- %s (%s)", dest, cur),
					fmt.Sprintf("+ %s (extract %s overwrites, archive content is invisible to the controller)", dest, src))
			}
			res.Diff = strings.Join(d, "\n")
		}
		return res
	}

	// remote_src：归档必须在远端存在
	if remoteSrc {
		out, bad := rc.exec(fmt.Sprintf("[ -e %s ]", shellquote.Quote(src)))
		if bad != nil {
			return bad
		}
		if out.Code != 0 {
			return Fail("remote archive not found: %s", src)
		}
	}

	// 目标目录存在性探测：新建目录登记回滚删除
	cur, bad := probePath(rc, dest)
	if bad != nil {
		return bad
	}
	if cur != "missing" && cur != "directory" {
		return Fail("%s exists and is not a directory", dest)
	}

	// 本地 src：读取并上传到远端临时路径。上传的临时副本是本模块的
	// 实现细节，成败路径都清理（此前默认与失败路径会在远端 /tmp 残留）
	remoteArc := src
	if !remoteSrc {
		data, err := os.ReadFile(resolveLocal(rc, src))
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
		rc.Rollback.RecordRemove(dest)
	}

	// 解压双路径：agent/push 通道优先原生（agent 侧 Go 实现，不依赖目标机
	// tar/unzip/xz，端点自建目标目录）；旧版 agent（404 哨兵）与 SSH 通道
	// 回退 shell 命令
	if nx, ok := rc.Conn.(conn.NativeExtractor); ok {
		if err := nx.NativeExtract(rc.Ctx, remoteArc, dest); err == nil {
			return &Result{Changed: true, Msg: fmt.Sprintf("extracted %s to %s (native)", src, dest)}
		} else if !errors.Is(err, conn.ErrNativeUnsupported) {
			return Fail("extract failed: %v", err)
		}
	}

	// zip 依赖 unzip，提前给出可读错误（原生路径无此依赖）
	if kind == "zip" {
		out, bad := rc.exec("command -v unzip >/dev/null 2>&1")
		if bad != nil {
			return bad
		}
		if out.Code != 0 {
			return Fail("%s", "target machine is missing unzip (installing the unzip package is required to extract .zip)")
		}
	}

	if out, bad := rc.exec(fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(dest))); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("failed to create directory: %s", firstLine(out.Stderr))
	}

	// 解压命令：tar 系列统一 -C dest；zip 用 unzip -o 覆盖解压
	script := fmt.Sprintf("tar -x%sf %s -C %s", tarFlag(kind), shellquote.Quote(remoteArc), shellquote.Quote(dest))
	if kind == "zip" {
		script = fmt.Sprintf("unzip -o %s -d %s", shellquote.Quote(remoteArc), shellquote.Quote(dest))
	}
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("extract failed: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("extracted %s to %s", src, dest)}
}

// runMembers 执行成员选取分发：本地归档按名检索成员（basename 或完整路径，
// 未命中报错），逐个拍平写入 dest/，复用 putFile 的校验和幂等与回滚登记。
func (m *UnarchiveModule) runMembers(rc *RunContext, src, kind, dest string, members []string, remoteSrc bool) *Result {
	if remoteSrc {
		return Fail("%s", "members requires a local src (the control node must inspect the archive to match entries)")
	}
	data, err := os.ReadFile(resolveLocal(rc, src))
	if err != nil {
		return Fail("failed to read local archive: %v", err)
	}
	sel, err := selectArchiveMembers(kind, data, members)
	if err != nil {
		return Fail("unarchive %s: %v", src, err)
	}

	cur, bad := probePath(rc, dest)
	if bad != nil {
		return bad
	}
	if cur != "missing" && cur != "directory" {
		return Fail("%s exists and is not a directory", dest)
	}
	if !rc.CheckMode && cur == "missing" {
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
		target := strings.TrimSuffix(dest, "/") + "/" + path.Base(mem.name)
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
