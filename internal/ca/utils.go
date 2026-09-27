package ca

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"

	"wdp/internal/fsatomic"
)

// parseCertificate 读取证书 PEM 文件并解析。
func parseCertificate(path string) (*x509.Certificate, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s is not a certificate PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

// writePEM 落盘单个 PEM 块（原子写经 fsatomic：临时文件 + chmod +
// fsync + rename + 目录同步）。rename 落盘文件的权限即临时文件的
// chmod 结果，重写已存在的宽松权限文件时自动收紧到 mode。
func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return err
	}
	return fsatomic.WriteFile(path, &buf, mode)
}

// randomSerial 生成 128 位随机序列号。失败直接报错：回退 UnixNano 是
// 可预测且可碰撞的（并发签发同纳秒即重号），序列号唯一性失败应当中止签发
// 而不是静默降级。
func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate certificate serial number: %w", err)
	}
	return n, nil
}
