package sshc

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// randHex 返回不可预测的 8 字节随机十六进制串（上传临时文件后缀），
// 避免 UnixNano 可预测路径被远端本机用户预创建符号链接劫持。
func randHex() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
