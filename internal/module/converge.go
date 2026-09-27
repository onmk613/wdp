package module

// 文件类模块的共用收敛点：owner/group 的 become 守卫与漂移探测、目标目录
// 预检/补建、归档成员逐个分发。这些语义此前以 3-4 份近似拷贝散落在
// file/fileops/lineinfile/geturl/unarchive/artifact——行为漂移已经实际发生
//（members 拍平的 basename 归一化不一致、新建目录漏登记回滚）。

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"wdp/internal/shellquote"
)

// requireBecomeForOwner 属主/属组设置要求提权（file/fileops/lineinfile/
// get_url 共用口径：显式报错而非静默跳过）。nil = 通过。
func requireBecomeForOwner(rc *RunContext, owner, group, dest string) *Result {
	if (owner != "" || group != "") && !rc.Become {
		return Fail("setting owner/group requires become: true (%s)", dest)
	}
	return nil
}

// ownerGroupDrift 探测远端属主/属组漂移（属性取不回按漂移处理）。
// putFile 与 get_url 的 check 预估共用；实跑校正走 fixAttrs。
func ownerGroupDrift(rc *RunContext, dest, owner, group string) (bool, *Result) {
	co, cg, ok, bad := remoteOwnerGroup(rc, dest)
	if bad != nil {
		return false, bad
	}
	return !ok || (owner != "" && co != owner) || (group != "" && cg != group), nil
}

// fixAttrs 实跑校正远端路径的权限/属主漂移：先探测属主漂移，再 chmod
// （mode 非 nil 时），最后按漂移 chown，返回各自是否发生了变更。
// putFile 内容未变的收尾、get_url 免下载收尾、lineinfile 属主收尾与
// file 模块 fixFileAttrs 的实跑分支共用（mode 传 nil 即"只校正属主"）。
// owner/group 均空时不动属主；属主漂移经 ownerGroupDrift 现场探测
// （探测在 chmod 之前，与 putFile/get_url 原实现时序一致）。
// check 预估分支各调用方口径不同（file 带 before→after diff 行展示，
// 见 fixFileAttrs 的 check 分支），不在此收敛。
func fixAttrs(rc *RunContext, path string, mode *fs.FileMode, owner, group string) (fixedMode, fixedOwner bool, bad *Result) {
	ownerDrift := false
	if owner != "" || group != "" {
		drift, obad := ownerGroupDrift(rc, path, owner, group)
		if obad != nil {
			return false, false, obad
		}
		ownerDrift = drift
	}
	if mode != nil {
		fixed, mbad := chmodIfDiffers(rc, path, int64(mode.Perm()))
		if mbad != nil {
			return false, false, mbad
		}
		fixedMode = fixed
	}
	if ownerDrift {
		if cbad := chownPath(rc, path, owner, group); cbad != nil {
			return false, false, cbad
		}
		fixedOwner = true
	}
	return fixedMode, fixedOwner, nil
}

// destDirState 目标目录预检：缺失（"missing"）或已是目录放行；存在但非
// 目录显式报错。归档解压（整包/members）与制品 members 分发共用。
// 返回 probePath 的形态串（missing/file/directory/link…），供调用方决定
// 回滚登记与补建。
func destDirState(rc *RunContext, dest string) (string, *Result) {
	cur, bad := probePath(rc, dest)
	if bad != nil {
		return "", bad
	}
	if cur != "missing" && cur != "directory" {
		return "", Fail("%s exists and is not a directory", dest)
	}
	return cur, nil
}

// mkdirDestMissing 目标目录缺失且非 check 模式时 mkdir -p 补建（check
// 模式只预估不落盘，与归档/制品路径同一约定）。新建目录是否登记回滚由
// 调用方经 recordMkdirRollback 决定。
func mkdirDestMissing(rc *RunContext, dest, cur string) *Result {
	if cur == "missing" && !rc.CheckMode {
		if out, bad := rc.exec(fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(dest))); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail("failed to create directory: %s", firstLine(out.Stderr))
		}
	}
	return nil
}

// recordMkdirRollback 新建目录（cur == missing）登记回滚删除。
func recordMkdirRollback(rc *RunContext, dest, cur string) {
	if rc.Rollback != nil && cur == "missing" {
		rc.Rollback.RecordRemove(dest)
	}
}

// distributeMemberFiles 逐个分发归档成员（拍平到 dest/，basename 命中；
// 权限沿用归档条目，forcedMode 在 hasMode 时覆盖）。unarchive 与 artifact
// 的 members 路径共用。basename 归一化统一处理 zip 条目名的反斜杠
// （Windows 打包器产物）——此前两份实现不一致，unarchive 侧会把反斜杠
// 当字面字符留在目标文件名里。目标路径用 path（远端恒为 POSIX 斜杠
// 语义），不用 filepath（Windows 控制端会引入反斜杠）。
// 返回 (changed, 失败结果)。
func distributeMemberFiles(rc *RunContext, dest string, sel []archiveMember, forcedMode int64, hasMode bool) (bool, *Result) {
	changed := false
	for _, mem := range sel {
		mode := mem.mode
		if mode == 0 {
			mode = 0o644
		}
		if hasMode {
			mode = forcedMode
		}
		target := strings.TrimSuffix(dest, "/") + "/" + path.Base(strings.ReplaceAll(mem.name, `\`, "/"))
		memChanged, res := putFile(rc, putFileOpts{data: mem.data, dest: target, mode: modePtr(fs.FileMode(mode))})
		if res != nil {
			if res.Failed {
				return changed, res
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
	return changed, nil
}
