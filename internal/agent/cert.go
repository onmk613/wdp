package agent

// 证书热更换（POST /cert）：控制端（web 控制台，持 enrollment CA）在证书
// 临期时把重签的新证书推给 agent——免重装/免重启。校验三道闸：
//   1. 仅 mTLS 模式可用（无认证/回环调试模式没有服务端证书可换）；
//   2. 新证书叶子公钥必须与 agent 现有私钥配对（换证不换钥：控制端 Renew
//      保留原钥重签；拿错证书换入会让后续握手全部失败，宁拒不换）；
//   3. 证书链必须能被 agent 已信任的 CA 池验证（防任意自签证书顶替）。
// 生效路径：tlsCertFile 原子落盘（tmp+fsync+rename）+ tlsMaterial 快照
// 原子换入——既有连接不受影响，新握手即用新证书（GetCertificate 回调
// 每次握手读当前快照）。

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"time"

	"wdp/internal/fsatomic"
)

// maxCertBodyBytes 换证请求体上限：PEM 证书链正常 1-5KiB，64KiB 已远超
// 合理范围，更大的只可能是误用或滥用。
const maxCertBodyBytes = 64 << 10

func (s *Server) handleCert(w http.ResponseWriter, r *http.Request) {
	// 校验与提交整体持锁：并发推送时两个请求各自基于同一快照校验后
	// 交错 Store/写文件，会让内存材料与磁盘短暂不一致（后写者覆盖）。
	// 三道闸（mTLS 模式 / 公钥配对 / 链可验证）基于同一临界区内的快照。
	s.certMu.Lock()
	defer s.certMu.Unlock()
	old := s.material.Load()
	if old == nil {
		http.Error(w, "certificate hot-swap requires mTLS mode", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCertBodyBytes+1))
	if err != nil {
		http.Error(w, "read body failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxCertBodyBytes {
		http.Error(w, "certificate body exceeds limit", http.StatusRequestEntityTooLarge)
		return
	}
	var chain [][]byte
	for rest := body; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			http.Error(w, "unexpected PEM block type: "+blk.Type, http.StatusBadRequest)
			return
		}
		chain = append(chain, blk.Bytes)
	}
	if len(chain) == 0 {
		http.Error(w, "no CERTIFICATE PEM blocks in body", http.StatusBadRequest)
		return
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		http.Error(w, "failed to parse certificate: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !pubKeyMatches(leaf.PublicKey, old.cert.PrivateKey) {
		http.Error(w, "certificate public key does not match the agent private key (renewal must keep the key)", http.StatusBadRequest)
		return
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     old.clientCAs,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		http.Error(w, "certificate not signed by a CA this agent trusts: "+err.Error(), http.StatusBadRequest)
		return
	}

	if s.tlsCertFile == "" {
		http.Error(w, "no certificate file path configured", http.StatusBadRequest)
		return
	}
	// 原子落盘（tmp+fsync+rename）收敛在 fsatomic：断电后要么旧文件
	// 要么完整新文件，不会留截断件——与 /upload 同语义
	if err := fsatomic.WriteFile(s.tlsCertFile, bytes.NewReader(normalizeCertPEM(chain)), 0o600); err != nil {
		http.Error(w, "failed to persist certificate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	pair := tls.Certificate{Certificate: chain, PrivateKey: old.cert.PrivateKey, Leaf: leaf}
	s.material.Store(&tlsMaterial{cert: pair, leaf: leaf, clientCAs: old.clientCAs, pins: old.pins})
	s.logInfo("server certificate hot-swapped: new expiry %s", leaf.NotAfter.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "not_after": leaf.NotAfter.Format(time.RFC3339)})
}

// pubKeyMatches 判断证书公钥与私钥是否配对（PKIX DER 逐字节比较，
// 覆盖 RSA/ECDSA/Ed25519 全部算法）。
func pubKeyMatches(pub crypto.PublicKey, priv any) bool {
	signer, ok := priv.(interface{ Public() crypto.PublicKey })
	if !ok {
		return false
	}
	a, err1 := x509.MarshalPKIXPublicKey(pub)
	b, err2 := x509.MarshalPKIXPublicKey(signer.Public())
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

// normalizeCertPEM 把 DER 链重新规范成标准 64 列 PEM（写入文件的是规范
// 形态，避免直接落请求原文——其中可能混入无关空白/注释块）。
func normalizeCertPEM(chain [][]byte) []byte {
	var out bytes.Buffer
	for _, der := range chain {
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	return out.Bytes()
}
