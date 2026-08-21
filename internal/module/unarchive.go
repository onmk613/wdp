package module

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"wdp/internal/connection"
	"wdp/internal/i18n"
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

// Desc 模块说明。
func (m *UnarchiveModule) Desc() string {
	return i18n.T("extract tar/zip archives into a remote directory", "解压 tar/zip 归档到远端目录")
}

// Params 参数文档。
func (m *UnarchiveModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "本地归档路径（playbook 相对路径基于 BaseDir 解析）"},
		{Name: "dest", Type: "string", Desc: "远端目标目录（不存在时自动创建）"},
		{Name: "remote_src", Type: "bool", Default: "false", Desc: "src 为远端路径（跳过上传，直接解压）"},
		{Name: "creates", Type: "string", Desc: "幂等守卫：该路径已存在则跳过任务（控制端看不到归档内容，重复执行需依赖此参数）"},
		{Name: "remove", Type: "bool", Default: "false", Desc: "解压后删除归档（仅本地 src：清理上传到远端的临时副本）"},
	}
}

// Example 示例任务。
func (m *UnarchiveModule) Example() string {
	return `# 分发包并解压（creates 守卫保证幂等：重复执行直接跳过）
- name: 部署应用包
  unarchive:
    src: files/myapp-1.2.3.tar.gz   # 本地归档，相对 playbook 目录
    dest: /opt/myapp
    creates: /opt/myapp/bin/myapp    # 已有该文件则跳过，避免重复解压覆盖

# 远端已有归档，直接解压
- name: 解压远端归档
  unarchive:
    src: /tmp/data.zip
    dest: /srv/data
    remote_src: true
    creates: /srv/data/README`
}

// Run 执行解压。
func (m *UnarchiveModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	src, ok := argStr(args, "src")
	if !ok || src == "" {
		return Fail("unarchive 需要 src 参数")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("unarchive 需要 dest 参数")
	}
	remoteSrc, _ := argBool(args, "remote_src")
	creates, _ := argStr(args, "creates")
	remove, _ := argBool(args, "remove")
	if remove && remoteSrc {
		return Fail("remove 仅支持本地 src（remote_src 时不会清理远端归档）")
	}

	kind := archiveKind(src)
	if kind == "" {
		return Fail("无法识别归档格式 %q（支持: .tar/.tgz/.tar.gz/.tar.xz/.txz/.zip）", src)
	}

	// creates 守卫：目标标记已存在则跳过（幂等）
	if creates != "" {
		out, bad := rc.exec(fmt.Sprintf("[ -e %s ]", shellquote.Quote(creates)))
		if bad != nil {
			return bad
		}
		if out.Code == 0 {
			return &Result{Msg: fmt.Sprintf("%s 已存在，跳过", creates)}
		}
	}

	// 控制端无法预知归档内容，check 模式只报告将执行解压
	if rc.CheckMode {
		res := &Result{Changed: true, Msg: fmt.Sprintf("[check] 将解压 %s 到 %s", src, dest)}
		if rc.DiffMode {
			cur, bad := probePath(rc, dest)
			if bad != nil {
				return bad
			}
			var d []string
			if cur == "missing" {
				d = append(d, fmt.Sprintf("+ %s（新建目录并解压 %s）", dest, src))
			} else {
				d = append(d, fmt.Sprintf("- %s（%s）", dest, cur),
					fmt.Sprintf("+ %s（解压 %s 覆盖，归档内容控制端不可见）", dest, src))
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
			return Fail("远端归档不存在: %s", src)
		}
	}

	// 目标目录存在性探测：新建目录登记回滚删除
	cur, bad := probePath(rc, dest)
	if bad != nil {
		return bad
	}
	if cur != "missing" && cur != "directory" {
		return Fail("%s 已存在且不是目录", dest)
	}

	// 本地 src：读取并上传到远端临时路径
	remoteArc := src
	if !remoteSrc {
		data, err := os.ReadFile(resolveLocal(rc, src))
		if err != nil {
			return Fail("读取本地归档失败: %v", err)
		}
		remoteArc = "/tmp/.wdp-arc-" + tempSuffix()
		if err := uploadBytes(rc, remoteArc, data, 0o600, true); err != nil {
			return Fail("上传归档失败: %v", err)
		}
	}

	if rc.Rollback != nil && cur == "missing" {
		rc.Rollback.RecordRemove(dest)
	}

	// 解压双路径：agent/push 通道优先原生（agent 侧 Go 实现，不依赖目标机
	// tar/unzip/xz，端点自建目标目录）；旧版 agent（404 哨兵）与 SSH 通道
	// 回退 shell 命令
	if nx, ok := rc.Conn.(connection.NativeExtractor); ok {
		if err := nx.NativeExtract(rc.Ctx, remoteArc, dest); err == nil {
			if remove && !remoteSrc {
				if out, bad := rc.exec(fmt.Sprintf("rm -f -- %s", shellquote.Quote(remoteArc))); bad != nil {
					return bad
				} else if out.Code != 0 {
					return Fail("清理归档失败: %s", firstLine(out.Stderr))
				}
			}
			return &Result{Changed: true, Msg: fmt.Sprintf("已解压 %s 到 %s（原生）", src, dest)}
		} else if !errors.Is(err, connection.ErrNativeUnsupported) {
			return Fail("解压失败: %v", err)
		}
	}

	// zip 依赖 unzip，提前给出可读错误（原生路径无此依赖）
	if kind == "zip" {
		out, bad := rc.exec("command -v unzip >/dev/null 2>&1")
		if bad != nil {
			return bad
		}
		if out.Code != 0 {
			return Fail("目标机缺少 unzip（.zip 解压需要先安装 unzip 包）")
		}
	}

	if out, bad := rc.exec(fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(dest))); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("创建目录失败: %s", firstLine(out.Stderr))
	}

	// 解压命令：tar 系列统一 -C dest；zip 用 unzip -o 覆盖解压
	script := fmt.Sprintf("tar -x%sf %s -C %s", tarFlag(kind), shellquote.Quote(remoteArc), shellquote.Quote(dest))
	if kind == "zip" {
		script = fmt.Sprintf("unzip -o %s -d %s", shellquote.Quote(remoteArc), shellquote.Quote(dest))
	}
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("解压失败: %s", firstLine(out.Stderr))
	}

	// remove：清理上传的临时归档（仅本地 src）
	if remove && !remoteSrc {
		if out, bad := rc.exec(fmt.Sprintf("rm -f -- %s", shellquote.Quote(remoteArc))); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail("清理归档失败: %s", firstLine(out.Stderr))
		}
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("已解压 %s 到 %s", src, dest)}
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
	sort.Strings(out)
	return out
}
