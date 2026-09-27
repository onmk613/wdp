package render

// safeMerge 是不就地改写的 merge：先深拷贝目标，再套用 sprig 的合并语义
// （同名键递归合并、src 覆盖 dst 的标量）。
//
// 为什么必须自己实现：sprig v3 的 merge 是 mergo.Merge(&dst, src)，直接写
// 入 dst。控制台的执行路径按主机并发 fanOut，各主机的变量域共享同一份
// chart values 嵌套 map——模板里的 merge 因此是"并发写同一 map"，
// Go 运行时会抛 concurrent map writes（fatal，不可 recover），
// 而即使侥幸不崩，主机之间也会互相污染配置。

import (
	"maps"

	"github.com/Masterminds/sprig/v3"
)

// sprigMerge 是 sprig 原生 merge 的实现（就地改写第一个参数）。
// 只允许作用于本文件拷贝出来的私有副本。comma-ok 断言：sprig 未来若
// 移除 merge 或改签名，得到 nil 而非包初始化期 panic——DefaultEngine
// 是无错误签名的包级构造，错误无法上抛，退化为 safeMerge 内的保守
// 合并兜底（见下）。
var sprigMerge, _ = sprig.TxtFuncMap()["merge"].(func(map[string]any, ...map[string]any) any)

// safeMerge 合并若干 map 并返回新 map（不改写任何入参）。
func safeMerge(dst map[string]any, srcs ...map[string]any) any {
	out := deepCopyMap(dst)
	if sprigMerge == nil {
		// sprig merge 不可用时的兜底：按"后者覆盖前者"浅合并（不递归）。
		// 语义比 sprig 弱（嵌套 map 整表覆盖而非逐键合并），但保持
		// "返回新 map、不改写入参"的硬约束，绝不 panic。
		for _, src := range srcs {
			maps.Copy(out, src)
		}
		return out
	}
	return sprigMerge(out, srcs...)
}

// deepCopyMap 递归拷贝 map/切片，标量按值复制。
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return v
	}
}
