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
func remoteChecksum(rc *RunContext, path string) (string, bool, *Result) {
	script := fmt.Sprintf(`p=%s
[ -f "$p" ] || exit 3
sha256sum "$p" 2>/dev/null || shasum -a 256 "$p" 2>/dev/null
exit $?`, shellquote.Quote(path))
	out, bad := rc.exec(script)
	if bad != nil {
		return "", false, bad
	}
	if out.Code == 3 {
		return "", false, nil
	}
	if out.Code != 0 {
		return "", false, Fail("读取远端校验和失败 rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	sum := strings.Fields(out.Stdout)
	if len(sum) == 0 {
		return "", false, Fail("无法解析校验和输出: %q", out.Stdout)
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
		return false, Fail("设置 owner/group 需要 become: true（%s）", dest)
	}
	sum := sha256hex(data)
	remote, exists, bad := remoteChecksum(rc, dest)
	if bad != nil {
		return false, bad
	}
	changed := !exists || remote != sum
	if rc.CheckMode {
		if !changed && hasMode {
			if cur, ok, mbad := remoteMode(rc, dest); mbad == nil && ok && cur != mode {
				changed = true
			}
		}
		var res *Result
		if changed {
			if !exists || remote != sum {
				res = &Result{Changed: true, Msg: fmt.Sprintf("[check] %s 将写入（%d 字节）", dest, len(data))}
				if rc.DiffMode {
					res.Diff = contentDiff(rc, dest, exists, string(data))
				}
			} else {
				res = &Result{Changed: true, Msg: fmt.Sprintf("[check] %s 内容一致，权限将校正", dest)}
				if rc.DiffMode {
					res.Diff = modeDiff(rc, dest, mode)
				}
			}
		} else {
			res = &Result{Changed: false, Msg: fmt.Sprintf("[check] %s 内容与权限一致", dest)}
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
		if (owner != "" || group != "") && rc.Become {
			if bad := chownPath(rc, dest, owner, group); bad != nil {
				return false, bad
			}
		}
		return changed, nil
	}
	if exists && backup {
		bak := fmt.Sprintf("%s.bak.%d", dest, time.Now().Unix())
		script := fmt.Sprintf("cp -a -- %s %s", shellquote.Quote(dest), shellquote.Quote(bak))
		if out, bad := rc.exec(script); bad != nil {
			return false, bad
		} else if out.Code != 0 {
			return false, Fail("备份失败: %s", firstLine(out.Stderr))
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
		return false, Fail("上传失败: %v", err)
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
		return false, Fail("路径不存在: %s", path)
	}
	script := fmt.Sprintf("chmod %04o %s", mode, shellquote.Quote(path))
	if out, bad := rc.exec(script); bad != nil {
		return false, bad
	} else if out.Code != 0 {
		return false, Fail("chmod 失败: %s", firstLine(out.Stderr))
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
		return 0, false, Fail("读取权限失败: %s", firstLine(out.Stderr))
	}
	var m int64
	if _, err := fmt.Sscanf(strings.TrimSpace(out.Stdout), "%o", &m); err != nil {
		return 0, false, Fail("无法解析权限 %q", out.Stdout)
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
		return Fail("chown 失败: %s", firstLine(out.Stderr))
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
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// maxDiffBytes 是内容 diff 的远端文件大小上限（超出仅提示）。
const maxDiffBytes = 1 << 20

// contentDiff 下载远端文件与目标内容做 unified diff（--diff 模式；
// check 专用只读路径，远端不存在时全部为新增行）。
func contentDiff(rc *RunContext, dest string, exists bool, want string) string {
	if !exists {
		return diffText("", want, "(远端不存在)", dest)
	}
	var buf bytes.Buffer
	if err := rc.Conn.DownloadFile(rc.Ctx, dest, &buf); err != nil {
		return fmt.Sprintf("(远端内容读取失败: %v)", err)
	}
	if buf.Len() > maxDiffBytes {
		return fmt.Sprintf("(远端文件 %d 字节超过 diff 上限，仅显示变更摘要)", buf.Len())
	}
	return diffText(buf.String(), want, "远端 "+dest, "目标 "+dest)
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
