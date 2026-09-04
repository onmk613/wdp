package model

// 结构字段自描述（`wdp schema`）的数据类型——与 module 的 ParamDoc 同一
// 思路：字段表由各解析包自带（文档长在实现旁边），对账测试防止文档与
// 解析器漂移。

// FieldDoc 描述一个 YAML 结构字段。GoField 是它填充的结构体字段名
// （model.Task / model.Host 的导出字段）——文档与结构体的机械关联：
// 对账测试据此反射校验"每个文档键真的被解析进声称的字段"，新增键
// 忘写解析或字段改名都会被点名。
type FieldDoc struct {
	Name    string
	Type    string
	Default string
	Desc    string
	GoField string
}

// FieldSection 是字段表的一个分组：按语义聚类渲染（如"条件与循环"），
// Example 是可直接粘贴的该组字段 YAML 片段（可空）。
type FieldSection struct {
	Title   string
	Fields  []FieldDoc
	Example string
}
