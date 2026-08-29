package executor

import (
	"maps"
)

// recordFacts 记录主机 facts（键级覆盖），供后续 play / 子 chart 作用域引用。
func (e *Executor) recordFacts(host string, facts map[string]any) {
	e.factsMu.Lock()
	defer e.factsMu.Unlock()
	cur, ok := e.facts[host]
	if !ok {
		cur = map[string]any{}
		e.facts[host] = cur
	}
	maps.Copy(cur, facts)
}

// seedFacts 把已积累的主机 facts 叠加进新建变量域（运行时数据覆盖静态层）。
func (e *Executor) seedFacts(host string, vars map[string]any) {
	e.factsMu.Lock()
	defer e.factsMu.Unlock()
	maps.Copy(vars, e.facts[host])
}
