package ca

// 根 CA 私钥的口令保护（opt-in）：根钥是整个纳管信任链的根——泄露即
// 可伪造任意主机身份，明文 0600 只防同机普通用户，防不了备份/磁盘镜像
// 外流。设 WDP_CA_PASS 环境变量后：
//   - Init 生成根 CA 时私钥以口令加密落盘（OpenSSL traditional PEM，
//     AES-256-CBC + PBKDF2，openssl 命令行可直接互操作）；
//   - LoadCAAt/Issue/Renew 读到加密块时用同一口令解密。
// 只加密根 CA 私钥：叶子私钥（逐主机 agent 钥匙、ctl 钥匙）保持明文——
// agent 侧经 tls.LoadX509KeyPair 加载，标准库不支持加密 PEM。
// 口令丢失 = 根 CA 不可用（只能重建 CA 并重新纳管），与明文私钥丢失的
// 后果方向一致但多了暴力破解成本，属纵深防御而非访问控制。

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"wdp/internal/fsatomic"
)

// PassEnv 是根 CA 私钥口令的环境变量名。
const PassEnv = "WDP_CA_PASS"

// Passphrase 返回当前配置的根 CA 私钥口令（未设置为空串）。
func Passphrase() string { return os.Getenv(PassEnv) }

// writeKeyEncrypted 落盘口令加密的根 CA 私钥（OpenSSL traditional PEM）。
// 使用标准库标记废弃的 EncryptPEMBlock：纯标准库无替代 API，该格式与
// openssl 互操作且仍在 TLS 生态广泛使用；威胁模型是静态窃取的纵深防御，
// 非 oracle 场景，AES-256-CBC + PBKDF2 足够。
func writeKeyEncrypted(path string, key crypto.Signer, pass []byte) error {
	der, _, err := marshalKey(key)
	if err != nil {
		return err
	}
	blk, err := x509.EncryptPEMBlock(rand.Reader, "PRIVATE KEY", der, pass, x509.PEMCipherAES256)
	if err != nil {
		return err
	}
	return writePEMBlock(path, blk, 0o600)
}

// writePEMBlock 落盘预构建的 PEM 块（与 writePEM 同一套原子写语义——
// fsatomic；供加密块使用——加密在块构造阶段完成，这里只负责落盘。
// 不能走 writePEM(der) 路线：加密块的口令头（Proc-Type/DEK-Info）在
// 块头部，重新按 der 编码会丢掉）。
func writePEMBlock(path string, blk *pem.Block, mode os.FileMode) error {
	var buf bytes.Buffer
	if err := pem.Encode(&buf, blk); err != nil {
		return err
	}
	return fsatomic.WriteFile(path, &buf, mode)
}

// decryptKeyBlock 解密口令保护的私钥 PEM 块（未加密块原样返回 DER）。
func decryptKeyBlock(blk *pem.Block, path string) ([]byte, error) {
	if !x509.IsEncryptedPEMBlock(blk) {
		return blk.Bytes, nil
	}
	pass := Passphrase()
	if pass == "" {
		return nil, fmt.Errorf("private key %s is passphrase-protected; set %s to use it", path, PassEnv)
	}
	der, err := x509.DecryptPEMBlock(blk, []byte(pass))
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt private key %s (wrong %s value?): %w", path, PassEnv, err)
	}
	return der, nil
}
