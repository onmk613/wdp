package executor

import (
	"slices"

	"wdp/internal/model"
)

func taskSelected(t *model.Task, opts Options) bool {
	if t.Block != nil {
		return blockSelected(t, opts, nil)
	}
	return tagsMatch(t.Tags, opts)
}

// tagsMatch 是 tags 过滤的核心判定（--tags 命中任一即选；--skip-tags
// 命中任一即排除）。
func tagsMatch(tags []string, opts Options) bool {
	if len(opts.Tags) > 0 {
		for _, want := range opts.Tags {
			if slices.Contains(tags, want) {
				return true
			}
		}
		return false
	}
	for _, skip := range opts.SkipTags {
		if slices.Contains(tags, skip) {
			return false
		}
	}
	return true
}

// effectiveTags 追加继承的祖先 tags（block 上的 tags 作用于全部子任务）。
func effectiveTags(t *model.Task, inherited []string) []string {
	if len(inherited) == 0 {
		return t.Tags
	}
	out := make([]string, 0, len(inherited)+len(t.Tags))
	out = append(out, inherited...)
	out = append(out, t.Tags...)
	return out
}

// blockSelected 判定任务（含 block 容器）是否被 tags 选中：
//   - 自身有效 tags（含继承）命中 skip-tags → 整组排除
//   - --tags 模式下自身或任一后代的有效 tags 命中即选中
//     （后代命中时容器选中，由 runSeq 在组内再逐子过滤）
func blockSelected(t *model.Task, opts Options, inherited []string) bool {
	eff := effectiveTags(t, inherited)
	for _, skip := range opts.SkipTags {
		if slices.Contains(eff, skip) {
			return false
		}
	}
	if len(opts.Tags) > 0 {
		for _, want := range opts.Tags {
			if slices.Contains(eff, want) {
				return true
			}
		}
		for _, group := range [][]*model.Task{t.Block, t.Rescue, t.Always} {
			for _, ch := range group {
				if blockSelected(ch, opts, eff) {
					return true
				}
			}
		}
		return false
	}
	return true
}
