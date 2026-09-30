package buildinfo

// BuildVersion 截断语义：纯 git 哈希截 7 位；带 .d<脏树哈希> 后缀时只
// 截短前段、保留后缀（后缀被截掉会让脏树重建与旧二进制同串，升级门控
// 误判已最新——实测踩过）。

import "testing"

func TestBuildVersion(t *testing.T) {
	cases := []struct {
		version, commit, want string
	}{
		{"1.2.3", "0123456789abcdef", "1.2.3-0123456"},              // 纯哈希：截 7
		{"1.2.3", "012345", "1.2.3-012345"},                         // 短哈希不动
		{"5163b63", "5163b63.de2b0cf6", "5163b63-5163b63.de2b0cf6"}, // 脏树后缀：保留
		{"9.9.9", "1234567890.abc1234", "9.9.9-1234567.abc1234"},    // 长前段+后缀：截前段
		{"0.0.1", "none", "0.0.1-none"},                             // 裸构建
	}
	for _, c := range cases {
		Version, Commit = c.version, c.commit
		if got := BuildVersion(); got != c.want {
			t.Errorf("BuildVersion(%s,%s) = %q, want %q", c.version, c.commit, got, c.want)
		}
	}
	Version, Commit = "0.0.1", "none"
}
