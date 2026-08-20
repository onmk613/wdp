package agent

import (
	"strings"
	"testing"
)

func TestIsLoopbackListen(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:7602": true,
		"localhost:7602": true,
		"[::1]:7602":     true,
		":7602":          false, // 空主机 = 全部网卡
		"0.0.0.0:7602":   false,
		"10.0.0.5:7602":  false,
		"192.168.1.2:80": false,
		"":               false,
		"7602":           false, // 非法地址按对外处理（安全侧默认）
	}
	for listen, want := range cases {
		if got := IsLoopbackListen(listen); got != want {
			t.Errorf("IsLoopbackListen(%q) = %v, want %v", listen, got, want)
		}
	}
}

// TestListenAndServeRefusesUnauthExposed 无认证 + 对外监听必须拒绝启动。
// 端口用 99999（非法值）：安全检查通过后 net.Listen 立即报错返回，测试不会阻塞。
func TestListenAndServeRefusesUnauthExposed(t *testing.T) {
	s := New("0.0.0.0:99999")
	err := s.ListenAndServe()
	if err == nil {
		t.Fatal("对外监听且无认证应拒绝启动")
	}
	if !strings.Contains(err.Error(), "allow-no-auth") {
		t.Fatalf("错误信息应指引 --allow-no-auth: %v", err)
	}

	// 显式放行后不再拒绝（后续错误来自非法端口而非安全检查）
	s2 := New("0.0.0.0:99999")
	s2.AllowNoAuth(true)
	err2 := s2.ListenAndServe()
	if err2 == nil || strings.Contains(err2.Error(), "拒绝启动") {
		t.Fatalf("allow-no-auth 应绕过安全检查，实际: %v", err2)
	}
}

// TestListenAndServeAllowsSafeConfigs 回环无认证 / 有 token / mTLS 均放行。
func TestListenAndServeAllowsSafeConfigs(t *testing.T) {
	s := New("127.0.0.1:99999")
	if err := s.ListenAndServe(); err == nil || strings.Contains(err.Error(), "拒绝启动") {
		t.Fatalf("回环无认证应允许: %v", err)
	}

	s2 := New("0.0.0.0:99999")
	s2.ConfigureAuth("secret", "", "", "", "")
	if err := s2.ListenAndServe(); err == nil || strings.Contains(err.Error(), "拒绝启动") {
		t.Fatalf("有 token 对外监听应允许: %v", err)
	}
}

// TestBecomeScriptPasswordNotInArgv 密码绝不能出现在最终脚本里
// （脚本作为 /bin/sh -c 的 argv，同机 ps 可见）。
func TestBecomeScriptPasswordNotInArgv(t *testing.T) {
	script, stdin := becomeScript("id -u", "deploy", "s3cr3t'pw", "extra-stdin")
	if strings.Contains(script, "s3cr3t") {
		t.Fatalf("密码泄漏进脚本 argv: %s", script)
	}
	if !strings.HasPrefix(script, "sudo -S -p '' -u 'deploy'") {
		t.Fatalf("应使用 sudo -S（stdin 传密码）: %s", script)
	}
	// 密码行在最前，业务 stdin 拼接其后（sudo -S 只消费首行）
	if stdin != "s3cr3t'pw\nextra-stdin" {
		t.Fatalf("stdin 组合错误: %q", stdin)
	}

	// 免密路径：sudo -n，stdin 原样
	script2, stdin2 := becomeScript("id", "root", "", "data")
	if script2 != "sudo -n -u 'root' -- /bin/sh -c 'id'" || stdin2 != "data" {
		t.Fatalf("免密路径错误: %q %q", script2, stdin2)
	}

	// 非提权：脚本与 stdin 原样返回
	script3, stdin3 := becomeScript("uptime", "", "pw", "data")
	if script3 != "uptime" || stdin3 != "data" {
		t.Fatalf("非提权路径错误: %q %q", script3, stdin3)
	}
}
