package console

// patchChartYAMLVersion：顶层 version 行替换（注释/其余内容逐字保真）、
// 无 version 行时插在 name 行后——与前端 patchChartYAMLVersion 同语义。

import (
	"strings"
	"testing"
)

func TestPatchChartYAMLVersion(t *testing.T) {
	// 替换首个顶层 version 行，注释与其余内容不动
	got := patchChartYAMLVersion("# 顶部注释\nname: demo # 行尾\nversion: 1.0.2\nrequired: [x]\n", "1.0.3")
	want := "# 顶部注释\nname: demo # 行尾\nversion: 1.0.3\nrequired: [x]\n"
	if got != want {
		t.Fatalf("替换:\ngot  %q\nwant %q", got, want)
	}
	// 无 version 行：插在 name 行后
	got = patchChartYAMLVersion("name: demo\n", "2.0.0")
	if got != "name: demo\nversion: 2.0.0\n" {
		t.Fatalf("插入: %q", got)
	}
	// 恰好一致时 PatchSpecChartVersion 不改写（指针内容不变）
	req := &SpecReq{Version: "1.0.3", Files: []SpecFile{
		{Path: "chart.yaml", Content: strp("name: d\nversion: 1.0.3\n")},
	}}
	before := *req.Files[0].Content
	PatchSpecChartVersion(req, "1.0.3")
	if *req.Files[0].Content != before {
		t.Fatal("版本一致时不应改写")
	}
	PatchSpecChartVersion(req, "1.0.4")
	if !strings.Contains(*req.Files[0].Content, "version: 1.0.4") {
		t.Fatalf("不一致时应改写: %q", *req.Files[0].Content)
	}
}

func strp(s string) *string { return &s }
