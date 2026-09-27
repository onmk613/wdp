package module

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"wdp/internal/conn"
	"wdp/internal/shellquote"
)

// uploadBytes 将字节流上传到远端路径。
//
// become 场景优先走提权写（conn.PrivilegedUploader）：UploadFile 以登录
// 用户落盘，非 root 登录 + `become: true` 写 /etc 之类目录必然
// permission denied——提权此前只作用于命令执行，对文件分发无效，而
// docs 明确要求 owner/group 配合 become 使用。连接不支持该能力时回退
// 旧行为（agent 通道本身以 root 运行，不受影响）。
func uploadBytes(rc *RunContext, dest string, data []byte, mode int64, hasMode bool) error {
	var fm fs.FileMode
	if hasMode {
		fm = fs.FileMode(mode)
	}
	if rc.Become {
		user := rc.BecomeUser
		if user == "" {
			user = "root"
		}
		if pu, ok := rc.Conn.(conn.PrivilegedUploader); ok {
			return pu.UploadFileAs(rc.Ctx, dest, bytes.NewReader(data), fm, user)
		}
	}
	return rc.Conn.UploadFile(rc.Ctx, dest, bytes.NewReader(data), fm)
}

// remoteChecksum 返回远端文件的 sha256（不存在时 ok=false）。
// rc=4：路径存在但不是普通文件（目录/设备等）——调用方必须失败而非
// 当作"不存在"，否则回滚日志会登记 RecordRemove，自动回滚时 rm -rf
// 掉既有目录。
func remoteChecksum(rc *RunContext, path string) (string, bool, *Result) {
	script := fmt.Sprintf(`p=%s
[ -e "$p" ] || exit 3
[ -f "$p" ] || exit 4
sha256sum "$p" 2>/dev/null || shasum -a 256 "$p" 2>/dev/null
exit $?`, shellquote.Quote(path))
	out, bad := rc.exec(script)
	if bad != nil {
		return "", false, bad
	}
	if out.Code == 3 {
		return "", false, nil
	}
	if out.Code == 4 {
		return "", false, Fail("remote path %s exists and is not a regular file (directory or device?)", path)
	}
	if out.Code != 0 {
		return "", false, Fail("failed to read remote checksum rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	sum := strings.Fields(out.Stdout)
	if len(sum) == 0 {
		return "", false, Fail("unable to parse checksum output: %q", out.Stdout)
	}
	return sum[0], true, nil
}

// sha256hex 计算本地数据校验和。
func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// fileSHA256 流式计算本地文件摘要（不把文件读进内存）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// putFileOpts 是 putFile 的参数集：mode 与 hasMode 合并为 *fs.FileMode
// （nil = 未指定权限，上传沿用通道缺省；非 nil = 始终显式下发，含调用方
// 的缺省口径 0644/0755），其余字段与原位置参数一一对应。
type putFileOpts struct {
	data   []byte
	dest   string
	mode   *fs.FileMode
	backup bool
	owner  string
	group  string
}

// modePtr 返回权限值的指针（构造 putFileOpts.mode 字面量用）。
func modePtr(m fs.FileMode) *fs.FileMode { return &m }

// putFile 将数据落盘到远端：校验和对比幂等、可选备份、可选属主设置。
// 返回 (changed, 失败结果)。check 模式下只读对比返回变更预估；
// diff 模式（--diff）追加内容级 unified diff。
func putFile(rc *RunContext, o putFileOpts) (bool, *Result) {
	data, dest, owner, group := o.data, o.dest, o.owner, o.group
	backup := o.backup
	// mode/hasMode 展开回 (权限值, 是否指定)：函数体按这两个变量走原逻辑
	var mode int64
	hasMode := o.mode != nil
	if hasMode {
		mode = int64(o.mode.Perm())
	}
	// 属主要求提权（requireBecomeForOwner：文件类模块共用口径）
	if bad := requireBecomeForOwner(rc, owner, group, dest); bad != nil {
		return false, bad
	}
	sum := sha256hex(data)
	remote, exists, bad := remoteChecksum(rc, dest)
	if bad != nil {
		return false, bad
	}
	changed := !exists || remote != sum
	// ownerDrift 探测属主漂移（此前 check 模式完全不评估属主，实跑修复了
	// 属主却报 changed=false，notify 不触发）。内容变更时实走"上传 + chown"
	// 路径无需探测（chown 无条件执行）；但 check 预估需要——实跑会改属主，
	// 预估与 diff 必须同样覆盖。远端不存在（新建文件）时无现状可比对，
	// 属主设置隐含在"写入"里。实跑的内容未变收尾改走 fixAttrs（探测在
	// 校正内部进行，不共用此处的预计算）。
	ownerDrift := false
	if (owner != "" || group != "") && exists && rc.CheckMode {
		drift, obad := ownerGroupDrift(rc, dest, owner, group)
		if obad != nil {
			return false, obad
		}
		ownerDrift = drift
	}
	if rc.CheckMode {
		contentChanged := changed
		// mode 漂移同样不因内容变更而跳过：实跑的内容变更路径以带 mode
		// 的上传落盘，check 只报内容 diff 会漏报权限变化（与实跑变更面
		// 不一致）。远端不存在时无现状可比，跳过。
		modeDrift := false
		var curMode int64
		if hasMode && exists {
			cur, ok, mbad := remoteMode(rc, dest)
			if mbad != nil {
				return false, mbad
			}
			if ok && cur != mode {
				modeDrift, curMode = true, cur
			}
		}
		attrDrift := ownerDrift || modeDrift
		changed = contentChanged || attrDrift
		var res *Result
		switch {
		case contentChanged:
			msg := fmt.Sprintf("[check] %s will be written (%d bytes)", dest, len(data))
			if attrDrift {
				msg += ", attributes will be corrected"
			}
			res = &Result{Changed: true, Msg: msg}
			if rc.DiffMode {
				parts := []string{contentDiff(rc, dest, exists, string(data))}
				if modeDrift {
					parts = append(parts, fmt.Sprintf("- mode: %04o\n+ mode: %04o", curMode, mode))
				}
				if ownerDrift {
					parts = append(parts, ownerDiff(rc, dest, owner, group)...)
				}
				res.Diff = strings.Join(parts, "\n")
			}
		case attrDrift:
			res = &Result{Changed: true, Msg: fmt.Sprintf("[check] %s content is unchanged, attributes will be corrected", dest)}
			if rc.DiffMode {
				res.Diff = modeDiff(rc, dest, mode)
			}
		default:
			res = &Result{Changed: false, Msg: fmt.Sprintf("[check] %s content and attributes are unchanged", dest)}
		}
		return changed, res
	}
	if !changed {
		// 内容未变时仍校正权限/属主（变更计入 changed，不再被丢弃）
		fixedMode, fixedOwner, bad := fixAttrs(rc, dest, o.mode, owner, group)
		if bad != nil {
			return false, bad
		}
		return fixedMode || fixedOwner, nil
	}
	if exists && backup {
		if bad := backupRemote(rc, dest); bad != nil {
			return false, bad
		}
	}
	// 变更前登记回滚动作（auto_rollback）：已存在 → 快照恢复；新建 → 回滚时删除
	if rc.Rollback != nil {
		if exists {
			rc.Rollback.Snapshot(rc, dest)
		} else {
			rc.Rollback.RecordRemove(dest)
		}
	}
	if err := uploadBytes(rc, dest, data, mode, hasMode); err != nil {
		return false, Fail("upload failed: %v", err)
	}
	if (owner != "" || group != "") && rc.Become {
		if bad := chownPath(rc, dest, owner, group); bad != nil {
			return false, bad
		}
	}
	return true, nil
}

// backupRemote 将远端路径备份为 <path>.bak.<UnixNano>（cp -a 保留属主与
// 权限；亚秒时间戳：同秒二次备份不再覆盖）。putFile 与 lineinfile 共用。
func backupRemote(rc *RunContext, path string) *Result {
	bak := fmt.Sprintf("%s.bak.%d", path, time.Now().UnixNano())
	script := fmt.Sprintf("cp -a -- %s %s", shellquote.Quote(path), shellquote.Quote(bak))
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("backup failed: %s", firstLine(out.Stderr))
	}
	return nil
}

// chmodIfDiffers 校正权限（八进制比较），返回是否发生变更。
func chmodIfDiffers(rc *RunContext, path string, mode int64) (bool, *Result) {
	cur, ok, bad := remoteMode(rc, path)
	if bad != nil {
		return false, bad
	}
	if ok && cur == mode {
		return false, nil
	}
	if !ok {
		return false, Fail("path does not exist: %s", path)
	}
	script := fmt.Sprintf("chmod %04o %s", mode, shellquote.Quote(path))
	if out, bad := rc.exec(script); bad != nil {
		return false, bad
	} else if out.Code != 0 {
		return false, Fail("chmod failed: %s", firstLine(out.Stderr))
	}
	return true, nil
}

// remoteMode 读取远端路径权限（GNU stat 优先，BSD stat 兜底）。
func remoteMode(rc *RunContext, path string) (int64, bool, *Result) {
	script := fmt.Sprintf(`p=%s
[ -e "$p" ] || exit 3
m=$(stat -c %%a "$p" 2>/dev/null || stat -f %%Lp "$p" 2>/dev/null)
[ -n "$m" ] || exit 4
printf '%%s' "$m"
exit 0`, shellquote.Quote(path))
	out, bad := rc.exec(script)
	if bad != nil {
		return 0, false, bad
	}
	switch out.Code {
	case 0:
	case 3:
		return 0, false, nil
	default:
		return 0, false, Fail("failed to read permission: %s", firstLine(out.Stderr))
	}
	var m int64
	if _, err := fmt.Sscanf(strings.TrimSpace(out.Stdout), "%o", &m); err != nil {
		return 0, false, Fail("unable to parse permission %q", out.Stdout)
	}
	return m, true, nil
}

// chownPath 设置属主/属组（需要提权）。
func chownPath(rc *RunContext, path, owner, group string) *Result {
	target := owner
	if group != "" {
		target = owner + ":" + group
	} else if owner == "" {
		target = ":" + group
	}
	script := fmt.Sprintf("chown %s %s", shellquote.Quote(target), shellquote.Quote(path))
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("chown failed: %s", firstLine(out.Stderr))
	}
	return nil
}

// modeDiff 生成权限校正的 diff 行（check/diff 模式下说明将发生的属性变更）。
func modeDiff(rc *RunContext, path string, want int64) string {
	cur, ok, bad := remoteMode(rc, path)
	if bad != nil || !ok {
		return fmt.Sprintf("- mode: unknown\n+ mode: %04o", want)
	}
	return fmt.Sprintf("- mode: %04o\n+ mode: %04o", cur, want)
}

// ownerDiff 生成属主校正的 diff 行（内容变更 + 属主漂移同时发生时的
// check/diff 输出；格式与 file 模块 fixFileAttrs 的 owner 行一致）。
func ownerDiff(rc *RunContext, path, owner, group string) []string {
	co, cg, ok, bad := remoteOwnerGroup(rc, path)
	if bad != nil || !ok {
		return []string{fmt.Sprintf("+ owner: %s:%s", owner, group)}
	}
	return []string{
		fmt.Sprintf("- owner: %s:%s", co, cg),
		fmt.Sprintf("+ owner: %s:%s", owner, group),
	}
}

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return strings.TrimSpace(before)
	}
	return strings.TrimSpace(s)
}

// maxDiffBytes 是内容 diff 的远端文件大小上限（超出仅提示）。
const maxDiffBytes = 1 << 20

// contentDiff 下载远端文件与目标内容做 unified diff（--diff 模式；
// check 专用只读路径，远端不存在时全部为新增行）。
// 下载经 cappedBuffer 流式封顶：旧实现先整份读进 bytes.Buffer 再比
// maxDiffBytes，判定在下完之后——远端一个几 GB 的文件就足以打爆控制端。
func contentDiff(rc *RunContext, dest string, exists bool, want string) string {
	if !exists {
		return diffText("", want, "(remote does not exist)", dest)
	}
	buf := &cappedBuffer{max: maxDiffBytes}
	if err := rc.Conn.DownloadFile(rc.Ctx, dest, buf); err != nil {
		return fmt.Sprintf("(failed to read remote content: %v)", err)
	}
	if buf.truncated() {
		return fmt.Sprintf("(remote file is over the %d byte diff limit; showing change summary only)", int64(maxDiffBytes))
	}
	return diffText(buf.buf.String(), want, "remote "+dest, "target "+dest)
}

// diffText 生成 unified diff（无差异返回空串）。
func diffText(old, want, from, to string) string {
	d := difflib.UnifiedDiff{
		A:        difflib.SplitLines(old),
		B:        difflib.SplitLines(want),
		FromFile: from,
		ToFile:   to,
		Context:  3,
	}
	s, err := difflib.GetUnifiedDiffString(d)
	if err != nil {
		return ""
	}
	return strings.TrimRight(s, "\n")
}
