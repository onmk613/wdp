package web

// 编辑器保存链路的数据一致性回归测试（曾出现的静默数据问题）：
//   - 空内容文件（新建空文件 / 清空已有文件）是合法编辑结果，必须按
//     提交值落盘，不得静默丢弃或回滚成底本内容
//   - deploy.yaml 被清空必须显式报错（不允许静默回滚底本）
//   - 编辑器保存/校验请求体按 specPayloadLimit（8MiB）放行：tgz 上传
//     允许的大 chart 在编辑器里不能保存失败

import (
	"net/http"
	"strings"
	"testing"

	"wdp/internal/store"
)

// TestSpecEmptyNewFileSaved 新建空文件：保存后包里必须存在该文件（0 字节）。
func TestSpecEmptyNewFileSaved(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	body := ideSpecBody("eapp2", "1.0.0", "name: eapp2\nversion: 1.0.0\n", "{}\n", ideDeployOK)
	body["files"] = append(body["files"].([]map[string]any),
		map[string]any{"path": "files/empty.conf", "content": ""},
		map[string]any{"path": "files/ok.conf", "content": "x"},
	)
	var app store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", body, &token, &app); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var spec AppSpec
	doJSON(t, h, "GET", "/api/apps/1/spec", nil, &token, &spec)
	found := false
	for _, f := range spec.Files {
		if f.Path == "files/empty.conf" {
			found = true
			if f.Binary || f.Content == nil || *f.Content != "" {
				t.Fatalf("空文件应作为 0 字节文本文件存在: %+v", f)
			}
		}
	}
	if !found {
		t.Fatalf("空内容新文件被静默丢弃: %+v", spec.Files)
	}
}

// TestSpecClearedFileStaysCleared 清空已有文件：保存后内容必须为空，
// 不得静默回滚成底本内容。
func TestSpecClearedFileStaysCleared(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	body := ideSpecBody("capp2", "1.0.0", "name: capp2\nversion: 1.0.0\n", "{}\n", ideDeployOK)
	body["files"] = append(body["files"].([]map[string]any),
		map[string]any{"path": "files/drop.txt", "content": "IMPORTANT"},
	)
	var app store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", body, &token, &app); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	part := map[string]any{
		"version":      "1.0.1",
		"base_version": "1.0.0",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: capp2\nversion: 1.0.1\n"},
			{"path": "files/drop.txt", "content": ""},
		},
	}
	if rec := do(t, h, "PUT", "/api/apps/1/spec", part, &token); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	var spec AppSpec
	doJSON(t, h, "GET", "/api/apps/1/spec?version=1.0.1", nil, &token, &spec)
	for _, f := range spec.Files {
		if f.Path == "files/drop.txt" {
			if f.Content == nil || *f.Content != "" {
				t.Fatalf("清空的文件被静默回滚成旧内容: %q", deref(f.Content))
			}
			if f.Binary {
				t.Fatalf("清空的文件不应变成二进制: %+v", f)
			}
		}
	}
}

// TestSpecClearedDeployRejected 清空 deploy.yaml：保存必须显式报错
// （不允许静默回滚底本后返回成功——编辑器侧会与实际制品状态漂移）。
func TestSpecClearedDeployRejected(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	var app store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", ideSpecBody("dapp2", "1.0.0",
		"name: dapp2\nversion: 1.0.0\n", "{}\n", ideDeployOK), &token, &app); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	part := map[string]any{
		"version":      "1.0.1",
		"base_version": "1.0.0",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: dapp2\nversion: 1.0.1\n"},
			{"path": "deploy.yaml", "content": ""},
		},
	}
	rec := do(t, h, "PUT", "/api/apps/1/spec", part, &token)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "deploy.yaml must not be empty") {
		t.Fatalf("清空 deploy.yaml 应显式 400 报错: %d %s", rec.Code, rec.Body)
	}

	// 校验端点同口径：归为一条 ERROR（门禁阻断）
	part["app_id"] = app.ID
	var resp issuesResp
	doJSON(t, h, "POST", "/api/apps/validate", part, &token, &resp)
	if len(resp.Issues) == 0 || resp.Issues[0].Level != "ERROR" ||
		!strings.Contains(resp.Issues[0].Msg, "deploy.yaml must not be empty") {
		t.Fatalf("validate 应报清空 deploy 的 ERROR: %+v", resp.Issues)
	}
}

// TestSpecRenameViaDeleteFiles 重命名的前端契约：IDE 的 ChartFS.rename
// 会在旧路径登记删除占位，保存请求 delete_files 带旧路径、files 带新
// 路径——落包后必须是「移动」而非「复制」（曾出现新旧并存）。
func TestSpecRenameViaDeleteFiles(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	body := ideSpecBody("rapp2", "1.0.0", "name: rapp2\nversion: 1.0.0\n", "{}\n", ideDeployOK)
	body["files"] = append(body["files"].([]map[string]any),
		map[string]any{"path": "files/old.conf", "content": "CONTENT"},
	)
	var app store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", body, &token, &app); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	part := map[string]any{
		"version":      "1.0.1",
		"base_version": "1.0.0",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: rapp2\nversion: 1.0.1\n"},
			{"path": "files/new.conf", "content": "CONTENT"},
		},
		"delete_files": []string{"files/old.conf"},
	}
	if rec := do(t, h, "PUT", "/api/apps/1/spec", part, &token); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	var spec AppSpec
	doJSON(t, h, "GET", "/api/apps/1/spec?version=1.0.1", nil, &token, &spec)
	hasOld, hasNew := false, false
	for _, f := range spec.Files {
		if f.Path == "files/old.conf" {
			hasOld = true
		}
		if f.Path == "files/new.conf" && deref(f.Content) == "CONTENT" {
			hasNew = true
		}
	}
	if hasOld || !hasNew {
		t.Fatalf("重命名应为移动（旧删新增）: old=%v new=%v", hasOld, hasNew)
	}
}

// TestSpecLargePayloadAccepted 保存/校验请求体按 8MiB 上限放行：
// 全局 decodeJSON 的 1MiB 曾把多文件 chart 卡死在「暂存成功、保存永久失败」。
func TestSpecLargePayloadAccepted(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	big := strings.Repeat("# padding line for size\n", 60*1024) // ~1.5MiB
	body := ideSpecBody("bigapp", "1.0.0", "name: bigapp\nversion: 1.0.0\n", "{}\n", ideDeployOK)
	body["files"] = append(body["files"].([]map[string]any),
		map[string]any{"path": "files/big.txt", "content": big},
	)
	var app store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", body, &token, &app); rec.Code != http.StatusCreated {
		t.Fatalf("1.5MiB 内容保存应成功（上限 8MiB）: %d %s", rec.Code, rec.Body)
	}
	// 校验端点同样放行
	var resp issuesResp
	if rec := doJSON(t, h, "POST", "/api/apps/validate", body, &token, &resp); rec.Code != http.StatusOK {
		t.Fatalf("大 payload 校验应 200: %d %s", rec.Code, rec.Body)
	}
}
