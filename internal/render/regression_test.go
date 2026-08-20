package render

import "testing"

// TestEnvFuncsRemoved 安全修复：模板不得读取控制端环境变量
// （env/expandenv 与 getHostByName 已从函数集移除，与 Helm 一致）。
func TestEnvFuncsRemoved(t *testing.T) {
	for _, tpl := range []string{
		`{{ env "HOME" }}`,
		`{{ expandenv "$HOME" }}`,
		`{{ getHostByName "localhost" }}`,
	} {
		if _, err := Render(tpl, nil); err == nil {
			t.Fatalf("模板 %q 应因函数被移除而报错", tpl)
		}
	}
}

// TestToYamlStillAvailable 移除 env 系列不得波及其它自有函数。
func TestToYamlStillAvailable(t *testing.T) {
	got, err := Render(`{{ to_yaml . }}`, map[string]any{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a: 1\n" {
		t.Fatalf("got %q", got)
	}
}
