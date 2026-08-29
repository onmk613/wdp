package push

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"time"
)

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomPort() int {
	// crypto/rand 取端口，不可预测（时间戳端口可被同网段探测者提前抢占/扫描）；
	// 避开常见服务端口段
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 20000 + int(time.Now().UnixNano()%40000) // 极端退化：熵源失败回退时间戳
	}
	return 20000 + int(binary.LittleEndian.Uint32(b[:]))%40000
}

// firstLines 取输出前 n 字符（多行错误诊断保留足够上下文）。
func firstLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
