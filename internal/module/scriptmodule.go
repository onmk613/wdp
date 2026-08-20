package module

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wdp/internal/shellquote"
)

// 脚本模块机制：chart 可携带自有模块（chart 根目录 modules/<名> 可执行脚本），
// 在内置注册表未命中时按目录约定发现并执行——应用包无需改 wdp 源码即可扩展模块。
//
// 契约：
//   - 脚本为任意可执行文件（上传后强制 0755），目标机需有对应解释器（sh/bash/python 均可）
//   - 参数注入：环境变量 WDP_MODULE_ARGS（任务参数 JSON 对象）、WDP_FREE_FORM（简写参数原文）
//   - check 模式注入 WDP_CHECK=1（脚本应自行返回预演结果且不变更）
//   - 结果约定：stdout 为 JSON 对象 {"changed":bool,"failed":bool,"msg":"..."} 时按其判定；
//     否则 rc==0 即 changed=true，stdout 原文作为 Msg
const scriptModuleDoc = `chart 自带脚本模块：modules/<名> 可执行脚本，参数经 WDP_MODULE_ARGS(JSON)/WDP_FREE_FORM 环境变量传入；stdout 输出 {"changed":..,"failed":..,"msg":..} 精确判定（缺省 rc==0 即变更）`

// FindScriptModule 在目录列表（当前 chart 作用域在前，chart 根在后）中查找
// modules/<name> 脚本模块；返回空串表示未找到。name 仅允许简单名（防路径逃逸）。
func FindScriptModule(dirs []string, name string) string {
	if name == "" || strings.ContainsAny(name, `/\`) || name[0] == '.' {
		return ""
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := filepath.Join(d, "modules", name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// RunScriptModule 上传并执行 chart 脚本模块（同 script 模块的临时路径自清理语义）。
func RunScriptModule(rc *RunContext, path string, args map[string]any, free string) *Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return Fail("读取脚本模块失败: %v", err)
	}
	if rc.CheckMode {
		// 脚本是外部代码，check 模式无法保证预演安全：仅当 chart.yaml
		// 显式声明 check_mode: supported 时才执行（注入 WDP_CHECK=1），
		// 否则跳过并报清晰提示，避免 --check 意外执行第三方脚本。
		if !rc.CheckScriptAllowed {
			return &Result{Skipped: true, Msg: fmt.Sprintf("[check] 跳过脚本模块（chart 未声明 check_mode: supported）: %s", path)}
		}
		return runScriptModuleCode(rc, data, args, free, true)
	}
	return runScriptModuleCode(rc, data, args, free, false)
}

func runScriptModuleCode(rc *RunContext, data []byte, args map[string]any, free string, check bool) *Result {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return Fail("模块参数序列化失败: %v", err)
	}
	remote := "/tmp/.wdp-mod-" + tempSuffix()
	if err := rc.Conn.UploadFile(rc.Ctx, remote, bytes.NewReader(data), 0o755); err != nil {
		return Fail("上传脚本模块失败: %v", err)
	}
	defer func() {
		_, _ = rc.exec(fmt.Sprintf("rm -f -- %s", shellquote.Quote(remote)))
	}()

	env := map[string]string{}
	for k, v := range rc.Env {
		env[k] = v
	}
	env["WDP_MODULE_ARGS"] = string(argsJSON)
	env["WDP_FREE_FORM"] = free
	if check {
		env["WDP_CHECK"] = "1"
	}

	script := fmt.Sprintf(`%s
rc=$?
rm -f -- %s
exit $rc`, shellquote.Quote(remote), shellquote.Quote(remote))
	out, bad := rc.execWithEnv(script, env)
	if bad != nil {
		return bad
	}
	res := parseScriptModuleOut(out.Stdout)
	res.Rc = out.Code
	res.Stdout = out.Stdout
	res.Stderr = out.Stderr
	if out.Code != 0 && !res.Failed {
		res.Failed = true
		res.Msg = fmt.Sprintf("脚本模块退出码 rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	return res
}

// parseScriptModuleOut 解析脚本 stdout 的结果约定（JSON 对象），失败回退默认判定。
func parseScriptModuleOut(stdout string) *Result {
	trimmed := strings.TrimSpace(stdout)
	if strings.HasPrefix(trimmed, "{") {
		var decl struct {
			Changed *bool  `json:"changed"`
			Failed  *bool  `json:"failed"`
			Msg     string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(trimmed), &decl); err == nil && (decl.Changed != nil || decl.Failed != nil || decl.Msg != "") {
			res := &Result{Changed: true, Msg: decl.Msg}
			if decl.Changed != nil {
				res.Changed = *decl.Changed
			}
			if decl.Failed != nil {
				res.Failed = *decl.Failed
			}
			if res.Msg == "" {
				res.Msg = fmt.Sprintf("changed=%v failed=%v", res.Changed, res.Failed)
			}
			return res
		}
	}
	return &Result{Changed: true, Msg: trimmed}
}
