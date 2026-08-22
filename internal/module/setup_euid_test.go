package module

// euid fact 回归：setup 采集的 .euid 是系统级任务 root 守卫
// （when: eq .euid 0）的依据；缺失时必须按 -1（非 root）处理，
// 而非 atoi("")==0 冒充 root 导致无 root 环境真跑系统级任务。

import (
	"testing"

	"wdp/internal/connection"
)

func setupWithOutput(t *testing.T, out string) map[string]any {
	t.Helper()
	rc, f := newTestRC(t)
	f.ExecFn = func(connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0, Stdout: out}, nil
	}
	res := (&SetupModule{}).Run(rc, nil, "")
	if res.Failed {
		t.Fatalf("setup: %s", res.Msg)
	}
	return res.Facts
}

func TestSetupEuidFact(t *testing.T) {
	base := "hostname=h\nkernel=k\narch=x86_64\nos_id=debian\ncpus=2\nmemory_mb=1024\n"

	cases := []struct {
		out  string
		want int
		desc string
	}{
		{base + "euid=0\n", 0, "root 环境采集为 0"},
		{base + "euid=1000\n", 1000, "普通用户采集为实际 uid"},
		{base, -1, "缺失 euid 行按 -1 非根处理（安全默认跳过）"},
	}
	for _, c := range cases {
		facts := setupWithOutput(t, c.out)
		euid, ok := facts["euid"].(int)
		if !ok {
			t.Fatalf("%s: euid fact 应为 int, got %T", c.desc, facts["euid"])
		}
		if euid != c.want {
			t.Fatalf("%s: got %d want %d", c.desc, euid, c.want)
		}
	}
}
