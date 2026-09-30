package module

// 模块文档的双语对账闸门（i18n 防腐烂）。
//
// 背景：模块文档（Desc/Params/Example）是 `wdp module`、`wdp module <名>`
// 与 web 编辑器补全共用的一套文案，全部经 i18n.T(en, zh) 成对给出。新增
// 模块时若只在 i18n.T 的第二个参数写中文、或干脆裸写字符串，英文侧会
// 静默回落到中文——编译、测试都通过，只有英语用户看得出来。
//
// 因此这里以「源码扫描」硬失败：
//   - 三个文档面（Desc / Params / Example）函数体内，不得出现未被
//     i18n.T 包裹的中文字符串字面量；
//   - 中文文案必须作为 i18n.T 的第二个实参出现（en 在前、zh 在后）。
//
// 与 build.sh 的脏树指纹、cmd/wdp-agent 的依赖边界断言同源：靠机器对账，
// 不靠记性。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// hasHan 报告字符串是否含汉字。
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// TestModuleDocsAreBilingual 扫描本包源码，断言模块文档面没有裸中文字面量。
func TestModuleDocsAreBilingual(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			switch fn.Name.Name {
			case "Desc", "Params", "Example":
			default:
				continue
			}
			checked++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				// i18n.T(en, zh) 的第二个实参是允许的中文位；其余裸中文字面量即违规
				if inSecondArgOfT(fn.Body, lit) {
					return true
				}
				if hasHan(lit.Value) {
					pos := fset.Position(lit.Pos())
					t.Errorf("%s: 模块文档出现未双语化的中文字面量 %s——"+
						"请写成 i18n.T(\"<english>\", \"<中文>\")", pos, truncate(lit.Value, 60))
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("未扫描到任何 Desc/Params/Example 方法——闸门失效（方法名或接收者形式变了？）")
	}
	t.Logf("已对账 %d 个文档面方法", checked)
}

// inSecondArgOfT 报告 lit 是否是某个 i18n.T(...) 调用的第二个实参。
func inSecondArgOfT(body *ast.BlockStmt, lit *ast.BasicLit) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "T" {
			return true
		}
		if arg, ok := call.Args[1].(*ast.BasicLit); ok && arg == lit {
			found = true
		}
		return true
	})
	return found
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestEnglishTextIsComplete 断言 i18n.T 的第一实参（英文）非空且不含汉字：
// 占位式的 i18n.T("", "中文") 或误把中文写成第一参会在这里失败。
func TestEnglishTextIsComplete(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("解析 %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "T" {
				return true
			}
			en, ok := call.Args[0].(*ast.BasicLit)
			if !ok || en.Kind != token.STRING {
				return true
			}
			if strings.Trim(en.Value, "`\"") == "" {
				t.Errorf("%s: i18n.T 的英文文案为空", fset.Position(en.Pos()))
			}
			if hasHan(en.Value) {
				t.Errorf("%s: i18n.T 的第一实参含汉字（英文位写成了中文？）",
					fset.Position(en.Pos()))
			}
			return true
		})
	}
}
