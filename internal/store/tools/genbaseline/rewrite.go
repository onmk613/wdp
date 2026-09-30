package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// rewriteBaselineGo 把生成的语句写回 internal/store/baseline.go。
//
// 字符串用解释串（json 转义与 Go 解释串兼容）而非原始串：MySQL 基线里有反引号
// 引用的保留字列名，而 Go 原始串无法包含反引号。
func rewriteBaselineGo(stmts map[string][]string) error {
	const head = `package store

// 网络库建表基线。
//
// 来源与维护方式：由 ` + "`go run ./internal/store/tools/genbaseline -write`" + ` 从
// **跑完历史迁移的 SQLite schema** 自动导出，因此不存在「手抄漏列」的可能。
//
// 改动 schema 的正确做法：
//
//  1. 新增一条 portableMigrations（三库通用增量），**不要**直接改本文件；
//  2. 基线只在「网络库首次建库」时执行一次，历史库不会重放它。
//
// 只有当希望新库一步到位（不再逐条重放增量）时，才重新生成基线，并确认
// portableMigrations 里的等价增量对已有库仍然成立。
// internal/store/baseline_test.go 会断言基线与迁移产物结构一致。
var (
`
	var b strings.Builder
	b.WriteString(head)
	for _, dialect := range []string{"postgres", "mysql"} {
		fmt.Fprintf(&b, "\t%sBaseline = []string{\n", dialect)
		for _, st := range stmts[dialect] {
			enc, err := json.Marshal(st)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "\t\t%s,\n", enc)
		}
		b.WriteString("\t}\n\n")
	}
	b.WriteString(")\n")
	return os.WriteFile("internal/store/baseline.go", []byte(b.String()), 0o644)
}
