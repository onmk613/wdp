package web

import (
	"testing"

	"wdp/internal/console"
)

func TestUnknownHostPattern(t *testing.T) {
	known := map[string]bool{"webservers": true, "db1": true}
	cases := []struct {
		pattern string
		unknown string
		ok      bool
	}{
		{"all", "", false},
		{"webservers", "", false},
		{"db1,db2", "db2", true},
		{"webservers,!staging", "", false},
		{"web*", "", false},                          // 通配视为已知
		{"webservers:&dbservers", "dbservers", true}, // 交集段未知
		{"nginx-web", "nginx-web", true},             // 旧 chart 应用名写法
	}
	for _, c := range cases {
		u, ok := console.UnknownHostPattern(c.pattern, known)
		if u != c.unknown || ok != c.ok {
			t.Errorf("pattern %q: got (%q,%v) want (%q,%v)", c.pattern, u, ok, c.unknown, c.ok)
		}
	}
}
