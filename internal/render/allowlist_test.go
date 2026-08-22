package render

import (
	"testing"

	"github.com/Masterminds/sprig/v3"
)

// TestSprigAllowlistValid 白名单里的每个名字都必须存在于 sprig 函数集
// （防止拼写错误导致文档承诺的函数静默缺失）。
func TestSprigAllowlistValid(t *testing.T) {
	full := sprig.TxtFuncMap()
	for _, name := range sprigAllowlist {
		if _, ok := full[name]; !ok {
			t.Errorf("白名单函数 %q 不存在于 sprig（拼写错误或上游移除）", name)
		}
	}
}

// TestDangerousSprigFuncsExposed 高危原语绝不可进入模板函数集。
func TestDangerousSprigFuncsExposed(t *testing.T) {
	exposed := DefaultEngine().funcsForTest()
	for _, name := range []string{
		"env", "expandenv", "getHostByName",
		"genPrivateKey", "genCA", "genSignedCert", "genSelfSignedCert",
		"buildCustomCertificate", "encryptAES", "decryptAES",
		"bcrypt", "htpasswd", "derivePassword", "randBytes",
	} {
		if _, ok := exposed[name]; ok {
			t.Errorf("高危函数 %q 不应暴露给模板", name)
		}
	}
}
