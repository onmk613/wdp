package chart

// values.schema.json：chart 级 values 结构校验（JSON Schema，Helm 同名惯例）。
// chart.yaml 的 required 列表只回答"缺不缺"，schema 还能回答"类型对不对、
// 取值合不合法"。校验发生在三处：run 的部署相位（与 required 同门控）、
// lint（合并 values + envs 叠加 + 子 chart 静态走查）、executor 子 chart
// 展开时（精确作用域，含引用 vars）。

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaFile 是 chart 根目录的 values schema 文件名（可选；子 chart 各自带）。
const SchemaFile = "values.schema.json"

// loadSchema 读取并编译 chart 根目录的 values.schema.json（文件不存在返回 nil）。
// JSON 语法或 schema 结构非法在加载期报错——与其他结构文件一致，不留到运行期。
func loadSchema(dir string) (*jsonschema.Schema, error) {
	data, err := os.ReadFile(filepath.Join(dir, SchemaFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", SchemaFile, err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(SchemaFile, doc); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", SchemaFile, err)
	}
	sch, err := compiler.Compile(SchemaFile)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", SchemaFile, err)
	}
	return sch, nil
}

// ValidateValuesSchema 校验给定 values 域是否符合本 chart 的 values.schema.json
// （无 schema 直接通过）。子 chart 场景 values 应为 SubScope 计算出的子作用域
// （含父注入的 global 与引用 vars——设置 additionalProperties: false 的 schema
// 需为它们留出声明）。
func (c *Chart) ValidateValuesSchema(values map[string]any) error {
	if c.schema == nil {
		return nil
	}
	if err := c.schema.Validate(values); err != nil {
		return fmt.Errorf("%s: values do not match %s: %v", c.Meta.Name, SchemaFile, err)
	}
	return nil
}

// ValidateSubchartsSchema 以给定 values 为起点静态走查全部子 chart 的 schema：
// 逐层用 SubScope 计算子作用域（子默认 → 父域 <子名> 子树 → global），在部署前
// 抓住"父 values 里 jdk 子树类型写错"这类跨层配置错误。引用任务上的 vars 是
// 运行期注入，不在此列——executor 展开时会用精确作用域再校验一次。
func (c *Chart) ValidateSubchartsSchema(values map[string]any) error {
	for _, name := range slices.Sorted(maps.Keys(c.Subs)) {
		sub := c.Subs[name]
		scope := SubScope(sub, values)
		if err := sub.ValidateValuesSchema(scope); err != nil {
			return fmt.Errorf("subchart %s: %w", name, err)
		}
		if err := sub.ValidateSubchartsSchema(scope); err != nil {
			return err
		}
	}
	return nil
}
