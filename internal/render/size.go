package render

// 容量单位换算：size（"10G" → 字节）与 humansize（字节 → "75.0Gi"）。
// facts 里的磁盘/内存统一为字节/MB 数值，比对与展示由模板函数完成，
// 避免每个 playbook 各写一套乘法。

import (
	"fmt"
	"strconv"
	"strings"
)

// sizeUnits 单位字符 → 二进制倍率。B/K/M/G/T/P（工业习惯按二进制计，
// 与 disk 等字段 *_bytes 的 1024 进制一致）；带 i（Ki/Mi）与 B 后缀
// （KB/MB/GiB）均接受，大小写不敏感。
var sizeUnits = map[string]float64{
	"b": 1,
	"k": 1 << 10, "ki": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
	"m": 1 << 20, "mi": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
	"g": 1 << 30, "gi": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
	"t": 1 << 40, "ti": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
	"p": 1 << 50, "pi": 1 << 50, "pb": 1 << 50, "pib": 1 << 50,
}

// ParseSize 解析容量字面量为字节数（int64）。接受 "10G" / "512Mi" /
// "1.5TB" / "1073741824"（无单位=字节）。非法输入返回错误——静默当 0
// 会让 `ge .disk.avail_bytes (size "10Q")` 恒假/恒真，误导排障。
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("size: empty value")
	}
	num, unit := s, ""
	for i, c := range s {
		if (c < '0' || c > '9') && c != '.' && c != '+' && c != '-' && c != 'e' {
			num, unit = s[:i], s[i:]
			break
		}
	}
	if unit == "" {
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("size: %q is not a number", s)
		}
		return n, nil
	}
	mul, ok := sizeUnits[strings.TrimSpace(unit)]
	if !ok {
		return 0, fmt.Errorf("size: unknown unit %q (use B/K/M/G/T/P, optionally Ki/Mi/Gi)", unit)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("size: %q is not a number", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("size: negative value %q", s)
	}
	return int64(f * mul), nil
}

// humanUnits humansize 的降序单位表（二进制，后缀 + 倍率并行）。
var humanUnits = []struct {
	suffix string
	mul    float64
}{
	{"Pi", 1 << 50}, {"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
}

// HumanSize 字节数 → 人读形态：80530636800 → "75.0Gi"；小于 1Ki 原样
// 输出字节数；负数原样透传（异常值不加工）。
func HumanSize(n int64) string {
	if n < 0 {
		return strconv.FormatInt(n, 10)
	}
	f := float64(n)
	for _, u := range humanUnits {
		if f >= u.mul {
			return strconv.FormatFloat(f/u.mul, 'f', 1, 64) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}
