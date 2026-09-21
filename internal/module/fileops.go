package module

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"wdp/internal/shellquote"
)

// uploadBytes 将字节流上传到远端路径。
func uploadBytes(rc *RunContext, dest string, data []byte, mode int64, hasMode bool) error {
	var fm fs.FileMode
	if hasMode {
		fm = fs.FileMode(mode)
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

// putFile 将数据落盘到远端：校验和对比幂等、可选备份、可选属主设置。
// 返回 (changed, 失败结果)。check 模式下只读对比返回变更预估；
// diff 模式（--diff）追加内容级 unified diff。
func putFile(rc *RunContext, data []byte, dest string, mode int64, backup, hasMode bool, owner, group string) (bool, *Result) {
	// 属主要求提权（与 file 模块行为一致：显式报错而非静默跳过）
	if (owner != "" || group != "") && !rc.Become {
		return false, Fail("setting owner/group requires become: true (%s)", dest)
	}
	sum := sha256hex(data)
	remote, exists, bad := remoteChecksum(rc, dest)
	if bad != nil {
		return false, bad
	}
	changed := !exists || remote != sum
	// ownerDrift 探测属主漂移（check 预演与实跑判定共用；此前 check 模式
	// 完全不评估属主，实跑修复了属主却报 changed=false，notify 不触发）
	ownerDrift := false
	if !changed && (owner != "" || group != "") {
		curOwner, curGroup, ok, obad := remoteOwnerGroup(rc, dest)
		if obad != nil {
			return false, obad
		}
		ownerDrift = !ok || (owner != "" && curOwner != owner) || (group != "" && curGroup != group)
	}
	if rc.CheckMode {
		if !changed && hasMode {
			if cur, ok, mbad := remoteMode(rc, dest); mbad != nil {
				return false, mbad
			} else if ok && cur != mode {
				changed = true
			}
		}
		if ownerDrift {
			changed = true
		}
		var res *Result
		if changed {
			if !exists || remote != sum {
				res = &Result{Changed: true, Msg: fmt.Sprintf("[check] %s will be written (%d bytes)", dest, len(data))}
				if rc.DiffMode {
					res.Diff = contentDiff(rc, dest, exists, string(data))
				}
			} else {
				res = &Result{Changed: true, Msg: fmt.Sprintf("[check] %s content is unchanged, attributes will be corrected", dest)}
				if rc.DiffMode {
					res.Diff = modeDiff(rc, dest, mode)
				}
			}
		} else {
			res = &Result{Changed: false, Msg: fmt.Sprintf("[check] %s content and attributes are unchanged", dest)}
		}
		return changed, res
	}
	if !changed {
		// 内容未变时仍校正权限/属主（变更计入 changed，不再被丢弃）
		if hasMode {
			if fixed, bad := chmodIfDiffers(rc, dest, mode); bad != nil {
				return false, bad
			} else if fixed {
				changed = true
			}
		}
		if ownerDrift {
			if bad := chownPath(rc, dest, owner, group); bad != nil {
				return false, bad
			}
			changed = true
		}
		return changed, nil
	}
	if exists && backup {
		bak := fmt.Sprintf("%s.bak.%d", dest, time.Now().UnixNano()) // 亚秒：同秒二次备份不再覆盖
		script := fmt.Sprintf("cp -a -- %s %s", shellquote.Quote(dest), shellquote.Quote(bak))
		if out, bad := rc.exec(script); bad != nil {
			return false, bad
		} else if out.Code != 0 {
			return false, Fail("backup failed: %s", firstLine(out.Stderr))
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
func contentDiff(rc *RunContext, dest string, exists bool, want string) string {
	if !exists {
		return diffText("", want, "(remote does not exist)", dest)
	}
	var buf bytes.Buffer
	if err := rc.Conn.DownloadFile(rc.Ctx, dest, &buf); err != nil {
		return fmt.Sprintf("(failed to read remote content: %v)", err)
	}
	if buf.Len() > maxDiffBytes {
		return fmt.Sprintf("(remote file is %d bytes, over the diff limit; showing change summary only)", buf.Len())
	}
	return diffText(buf.String(), want, "remote "+dest, "target "+dest)
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
