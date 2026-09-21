//go:build !darwin

package agent

// 非 darwin 平台的兜底实现（生产环境是 Linux /proc 路径，这些不会被调用；
// 其它试验平台返回未支持，采集器输出空集而非报错）。

func darwinMemTotal() (uint64, bool) { return 0, false }

func darwinBootSecs() (uint64, bool) { return 0, false }
