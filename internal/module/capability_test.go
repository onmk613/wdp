package module

import "testing"

// TestRollbackCapabilities 验证模块自声明的回滚/只读能力
// （chart 可逆性评估的数据来源，取代跨包硬编码名单）。
func TestRollbackCapabilities(t *testing.T) {
	full := []string{"copy", "template", "file"}
	for _, name := range full {
		if got := RollbackCapabilityOf(name); got != RollbackFull {
			t.Errorf("%s 应为 RollbackFull, got %v", name, got)
		}
	}
	if got := RollbackCapabilityOf("unarchive"); got != RollbackPartial {
		t.Errorf("unarchive 应为 RollbackPartial, got %v", got)
	}
	for _, name := range []string{"shell", "script", "package", "service", "user"} {
		if got := RollbackCapabilityOf(name); got != RollbackNone {
			t.Errorf("%s 应为 RollbackNone, got %v", name, got)
		}
	}
	if !IsReadOnlyModule("setup") {
		t.Error("setup 应为只读")
	}
	if IsReadOnlyModule("copy") || IsReadOnlyModule("shell") {
		t.Error("copy/shell 不应被标为只读")
	}
	if RollbackCapabilityOf("no-such-module") != RollbackNone {
		t.Error("未知模块应视为 RollbackNone")
	}
}
