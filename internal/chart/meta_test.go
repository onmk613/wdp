package chart

import "testing"

// 回归：chart name 会拼进远端 marker 路径与 uninstall 清理命令，
// 路径穿越写法必须在加载入口被拒绝（曾可达 rm -rf 越界删除）。
func TestValidateMetaRejectsPathTraversal(t *testing.T) {
	bad := []string{"", "a/../../victim", "a/b", "..", ".", "../x", "a\\b", "/abs", "sp ace"}
	for _, name := range bad {
		m := &Meta{Name: name}
		if err := validateMeta(m); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
	good := []string{"my-app", "app_1", "App.2", "x"}
	for _, name := range good {
		if err := validateMeta(&Meta{Name: name}); err != nil {
			t.Errorf("name %q should be accepted: %v", name, err)
		}
	}
}

// marker_dir 必须是干净的绝对路径且不是根目录。
func TestValidateMetaMarkerDir(t *testing.T) {
	for _, dir := range []string{"relative/path", "/var/lib/../wdp", "/", "/a/.."} {
		if err := validateMeta(&Meta{Name: "app", MarkerDir: dir}); err == nil {
			t.Errorf("marker_dir %q should be rejected", dir)
		}
	}
	for _, dir := range []string{"", "/var/lib/wdp", "/tmp"} {
		if err := validateMeta(&Meta{Name: "app", MarkerDir: dir}); err != nil {
			t.Errorf("marker_dir %q should be accepted: %v", dir, err)
		}
	}
}
