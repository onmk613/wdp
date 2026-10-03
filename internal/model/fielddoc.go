package model

// 结构字段自描述（`wdp schema`）的数据类型——与 module 的 ParamDoc 同一
// 思路：字段表由各解析包自带（文档长在实现旁边），对账测试防止文档与
// 解析器漂移。

// FieldDoc 描述一个 YAML 结构字段。GoField 是它填充的结构体字段名
// （model.Task / model.Host 的导出字段）——文档与结构体的机械关联：
// 对账测试据此反射校验"每个文档键真的被解析进声称的字段"，新增键
// 忘写解析或字段改名都会被点名。
//
// 命名口径：JSON tag 统一 snake_case（此前 /api/schema 按 Go 导出名序列化
// 成 PascalCase，与全目录 API 的 snake_case 口径混用，现统一；CLI 的
// `wdp schema` 只做文本渲染，不受影响）。
type FieldDoc struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default"`
	Desc    string `json:"desc"`
	GoField string `json:"go_field"`
}

// FieldSection 是字段表的一个分组：按语义聚类渲染（如"条件与循环"），
// Example 是可直接粘贴的该组字段 YAML 片段（可空）。
type FieldSection struct {
	Title   string     `json:"title"`
	Fields  []FieldDoc `json:"fields"`
	Example string     `json:"example"`
}
