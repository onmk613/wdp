package ca

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
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

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	// 原子写：临时文件 + rename（O_TRUNC 原地重写，进程在截断后写完前
	// 崩溃或磁盘满即私钥/证书永久损坏）。rename 落盘文件的权限即临时文件
	// 的 chmod 结果，重写已存在的宽松权限文件时自动收紧到 mode
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wdp-ca-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	abort := func(e error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return e
	}
	if err := pem.Encode(tmp, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return abort(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return abort(err)
	}
	if err := tmp.Sync(); err != nil {
		return abort(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
