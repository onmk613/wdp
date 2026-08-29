// Package pushcerts 管理 push 连接的会话级 mTLS 材料：会话 CA 落盘于
// wdp.cfg [agent].push_ca_dir（默认 ~/.wdp/push-ca），与 wdp ca init/issue
// 同一套签发逻辑，全部主机共享，不按主机签发。CA 有效期 1 天：跨进程
// 复用同一信任链，可用 `wdp ca show` 检视；到期或轮换时重建。
//
// 证书轮换（wdp.cfg [agent].cert_rotate_min，缺省不轮换）：证书对为全部
// 主机共享，配置轮换周期可压缩其暴露窗口（被攻破主机配合流量劫持冒充
// 他机的有效期缩短为一个周期）。仓库到期由首个使用者换新代信任链
// （gen 单调递增，每代独立），其余存活主机在各自下一次任务前惰性迁移；
// 已完成自销毁的主机不再有任务，自然永不轮换。
//
// 传输引导（上传、拉起 agent、直连）在 internal/conn/push。
package pushcerts

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wdp/internal/ca"
	"wdp/internal/conn"
)

// 会话 CA 产物名（push-server / push-control 叶子证书 + ca.crt/ca.key）。
const (
	pushServerName  = "push-server"
	pushControlName = "push-control"
)

// Session 是一次 push 会话的 mTLS 材料（从会话目录加载的 PEM）：
// Server* 与 CA 上传目标机，Client* 仅控制端持有。
type Session struct {
	CACertPEM     []byte
	ServerCertPEM []byte
	ServerKeyPEM  []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte
}

// store 进程级证书仓库：push 全部主机共享当前代材料（gen 单调递增，
// 每代独立信任链）。dir 记录加载来源（多 wdp 进程共用时会话目录一致）。
var store struct {
	sync.Mutex
	certs *Session
	dir   string
	gen   uint64
	at    time.Time
}

// Material 返回当前代材料与代数（首次调用生成；配置轮换且已到期先换新）。
// dc 为组合根注入的默认值（轮换周期与会话目录取自其中；nil = 内置默认）。
func Material(dc *conn.Defaults) (*Session, uint64, error) {
	store.Lock()
	defer store.Unlock()
	if err := refreshDueLocked(dc); err != nil {
		return nil, 0, err
	}
	return store.certs, store.gen, nil
}

// Reset 清空进程级仓库（测试隔离用）。
func Reset() {
	store.Lock()
	store.certs, store.dir, store.gen, store.at = nil, "", 0, time.Time{}
	store.Unlock()
}

// refreshDueLocked 材料缺失或超过轮换周期时重新生成（调用方持有锁）。
// 与 wdp ca init/issue 同一套逻辑：会话 CA 落盘可复用（1 天有效期内跨进程
// 共享信任链），轮换/到期重建目录即换代。
func refreshDueLocked(dc *conn.Defaults) error {
	dir := dc.PushCADirOrDefault()
	interval := dc.AgentCertRotateMinOrDefault()
	if store.certs != nil && store.dir == dir &&
		(interval <= 0 || time.Since(store.at) <= time.Duration(interval)*time.Minute) {
		return nil
	}
	certs, err := LoadOrCreate(dir)
	if err != nil {
		return err
	}
	store.certs, store.dir = certs, dir
	store.gen, store.at = store.gen+1, time.Now()
	return nil
}

// LoadOrCreate 加载或重建 push 会话 CA（全部产物在 dir 内）：
//   - CA 缺失/过期/密钥不匹配 → 清掉旧产物重新 init（1 天，CN=wdp-push-ca）
//   - 叶子证书缺失或过期 → 按同名补签（server: SAN=wdp-push-server；
//     client: CN=wdp-push-control）
//
// 产物即普通 wdp ca 产物，`wdp ca show <dir>/ca.crt` 可直接检视。
func LoadOrCreate(dir string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	caCrt, caKey := filepath.Join(dir, ca.DefaultCAFile), filepath.Join(dir, ca.DefaultKeyFile)
	cert, _, loadErr := ca.LoadCAAt(caCrt, caKey)
	switch {
	case loadErr == nil && time.Now().Before(cert.NotAfter):
		// 有效 CA：直接复用
	case loadErr == nil || errors.Is(loadErr, fs.ErrNotExist):
		// 过期或缺失：锁内删旧建新。此前解析失败（损坏/证书私钥不配对）
		// 也静默走删除重建——可疑材料应交人工检视而非自动销毁，且删旧
		// 建新非原子，两个 wdp 进程并发重建会互删对方产物
		if err := withCALock(dir, func() error {
			// 锁内二次确认：竞争者可能已完成重建
			c2, _, e2 := ca.LoadCAAt(caCrt, caKey)
			if e2 == nil && time.Now().Before(c2.NotAfter) {
				return nil
			}
			for _, f := range []string{ca.DefaultCAFile, ca.DefaultKeyFile,
				pushServerName + ".crt", pushServerName + ".key",
				pushControlName + ".crt", pushControlName + ".key"} {
				_ = os.Remove(filepath.Join(dir, f))
			}
			_, _, _, err := ca.Init(ca.InitOptions{
				Dir:     dir,
				Subject: ca.Subject{CN: "wdp-push-ca"},
			})
			return err
		}); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("push CA material in %s is unreadable (corrupt or mismatched cert/key): %v; inspect it, then remove the directory manually to rebuild", dir, loadErr)
	}
	if err := ensureLeaf(dir, pushServerName, ca.ProfileServer); err != nil {
		return nil, err
	}
	if err := ensureLeaf(dir, pushControlName, ca.ProfileClient); err != nil {
		return nil, err
	}
	out := &Session{}
	var err error
	if out.CACertPEM, err = os.ReadFile(caCrt); err != nil {
		return nil, err
	}
	if out.ServerCertPEM, out.ServerKeyPEM, err = readPair(dir, pushServerName); err != nil {
		return nil, err
	}
	if out.ClientCertPEM, out.ClientKeyPEM, err = readPair(dir, pushControlName); err != nil {
		return nil, err
	}
	return out, nil
}

// withCALock 用 O_EXCL 锁文件串行化多进程的 CA 重建；进程崩溃残留的
// 陈旧锁（>1 分钟）自动抢占。
func withCALock(dir string, fn func() error) error {
	lock := filepath.Join(dir, ".wdp-ca.lock")
	deadline := time.Now().Add(15 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			defer os.Remove(lock)
			_ = f.Close()
			return fn()
		}
		if fi, statErr := os.Stat(lock); statErr == nil && time.Since(fi.ModTime()) > time.Minute {
			_ = os.Remove(lock) // 陈旧锁：持锁进程已死
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("push CA dir %s is locked by another wdp process", dir)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ensureLeaf 叶子证书存在且未过期则复用，否则补签（SAN = 同名 DNS）。
func ensureLeaf(dir, name string, profile ca.Profile) error {
	certPath := filepath.Join(dir, name+".crt")
	if cert, err := parseCertFile(certPath); err == nil && time.Now().Before(cert.NotAfter) {
		return nil
	}
	_, _, _, err := ca.Issue(ca.IssueOptions{
		Dir:     dir,
		Profile: profile,
		SANs:    []string{name},
	}, name)
	return err
}

// readPair 读取证书/私钥 PEM。
func readPair(dir, name string) (crtPEM, keyPEM []byte, err error) {
	if crtPEM, err = os.ReadFile(filepath.Join(dir, name+".crt")); err != nil {
		return nil, nil, err
	}
	if keyPEM, err = os.ReadFile(filepath.Join(dir, name+".key")); err != nil {
		return nil, nil, err
	}
	return crtPEM, keyPEM, nil
}

// parseCertFile 解析证书文件（不存在/损坏返回错误，调用方按需补签）。
func parseCertFile(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s is not a certificate PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
}
