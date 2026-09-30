package agent

import "testing"

// TestParsePins 两种传参形态都要接受：StringArray 重复传参（unit 下发的
// 正确形态）与历史逗号拼接形态。后者曾导致 agent 把
// "sha256:A,sha256:B" 当单个指纹解析失败、启动即退。
func TestParsePins(t *testing.T) {
	fpA := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fpB := "2222222222222222222222222222222222222222222222222222222222222222"

	// 重复传参
	m, err := parsePins([]string{fpA, "sha256:" + fpB})
	if err != nil {
		t.Fatalf("重复传参应通过: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("应有两个 pin: %v", m)
	}

	// 逗号拼接（历史形态）
	m2, err := parsePins([]string{fpA + "," + "sha256:" + fpB})
	if err != nil {
		t.Fatalf("逗号拼接应容忍: %v", err)
	}
	if len(m2) != 2 {
		t.Fatalf("逗号拼接应拆出两个 pin: %v", m2)
	}

	// 混合
	if m3, err := parsePins([]string{fpA, "sha256:" + fpB + "," + fpA}); err != nil || len(m3) != 2 {
		t.Fatalf("混合形态应通过且去重: %v %v", m3, err)
	}

	// 非法值仍要报错（含空段）
	if _, err := parsePins([]string{fpA + ","}); err == nil {
		t.Fatal("尾逗号空段应报错")
	}
	if _, err := parsePins([]string{"not-a-fingerprint"}); err == nil {
		t.Fatal("非法指纹应报错")
	}
}
