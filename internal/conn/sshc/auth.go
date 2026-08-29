package sshc

import (
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"wdp/internal/model"
)

// authMethods 组装认证链：身份文件（显式 key_path → ~/.ssh/config 身份
// 文件 → ssh-agent → 默认密钥，每个身份文件自动附带伴随证书
// <私钥>-cert.pub）→ 密码（同时启用 keyboard-interactive）。
// 注意全部公钥签名者必须合并进**单个** ssh.PublicKeys 方法：x/crypto
// 客户端按方法名记录 tried，第一个 publickey 方法失败后其余 publickey
// 方法会被整体跳过——拆成多个方法时"多密钥依次尝试"实际不生效。
// 返回的 cleanup 释放认证过程中打开的资源（agent unix 连接），
// 调用方必须在 ssh.Dial 返回后调用（认证已同步完成）。
// warns 收集"文件存在但解析失败"的私钥诊断（口令错误/密钥损坏），
// 供连接失败时提示根因；文件不存在（如默认密钥未生成）不算异常。
func authMethods(h *model.Host) ([]ssh.AuthMethod, func(), []string) {
	var cleanups []func()
	var warns []string
	cleanup := func() {
		for _, f := range cleanups {
			f()
		}
	}
	passphrase := model.Secret(h.KeyPassphrase, h.KeyPassphraseEnv)

	// parseKeySigners 解析单个身份文件：裸签名者 + 伴随证书签名者
	// （服务器只信任 CA 签发证书、裸公钥不在 authorized_keys 时靠证书认证，
	// 与交互 ssh 自动加载 <私钥>-cert.pub 的行为一致）。
	parseKeySigners := func(p string) []ssh.Signer {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			// 加密私钥：带口令重试
			if pe, ok := err.(*ssh.PassphraseMissingError); ok && pe != nil && passphrase != "" {
				s2, err2 := ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
				if err2 != nil {
					warns = append(warns, fmt.Sprintf("%s: %v", p, err2))
					return nil
				}
				signer = s2
			} else {
				warns = append(warns, fmt.Sprintf("%s: %v", p, err))
				return nil
			}
		}
		signers := []ssh.Signer{signer}
		if cert, warn := certSignerFor(p, signer); cert != nil {
			signers = append(signers, cert)
		} else if warn != "" {
			warns = append(warns, warn)
		}
		return signers
	}

	// 身份文件来源（对齐 OpenSSH）：显式 key_path > ssh config 匹配块的
	// IdentityFile 累积 > 内置默认密钥（配置了 IdentityFile 即不再补默认）。
	// 默认密钥单独返回、由调用方排在 agent 之后，与旧认证链顺序一致。
	keyPaths, defaultKeys := keyFilePaths(h)
	var signers []ssh.Signer
	for _, p := range keyPaths {
		signers = append(signers, parseKeySigners(p)...)
	}

	// ssh-agent（SSH_AUTH_SOCK）：签名者即刻取回并入同一方法（回调形式
	// 是独立的 publickey 方法，签名者非空时才会排在文件密钥后生效，
	// 见函数注释）
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentCl := agent.NewClient(conn)
			if agentSigners, err := agentCl.Signers(); err == nil && len(agentSigners) > 0 {
				signers = append(signers, agentSigners...)
				cleanups = append(cleanups, func() { _ = conn.Close() })
			} else {
				_ = conn.Close() // 无可用 signer，立即释放
			}
		}
	}
	for _, p := range defaultKeys {
		signers = append(signers, parseKeySigners(p)...)
	}

	var methods []ssh.AuthMethod
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	// 密码 + keyboard-interactive
	if pw := model.Secret(h.Password, h.PasswordEnv); pw != "" {
		methods = append(methods,
			ssh.Password(pw),
			ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = pw
				}
				return answers, nil
			}),
		)
	}
	return methods, cleanup, warns
}

// keyFilePaths 归集身份文件来源：primary 为优先尝试的显式/配置文件，
// defaults 为内置默认密钥（primary 为空时才有，由调用方排在 agent 后）。
func keyFilePaths(h *model.Host) (primary, defaults []string) {
	switch {
	case h.KeyPath != "":
		return []string{h.KeyPath}, nil
	case len(h.IdentityFiles) > 0:
		return h.IdentityFiles, nil
	default:
		home, _ := os.UserHomeDir()
		return nil, []string{
			home + "/.ssh/id_ed25519",
			home + "/.ssh/id_ecdsa",
			home + "/.ssh/id_rsa",
		}
	}
}

// certSignerFor 加载身份文件的伴随证书（OpenSSH 约定：<私钥>-cert.pub，
// 已带 .pub 后缀时替换为 -cert.pub）并与私钥组合为证书签名者。
// 证书文件不存在是常态（返回 nil, ""）；存在但无法使用（损坏/与私钥
// 不配对）返回诊断供连接失败时提示根因。
func certSignerFor(keyPath string, signer ssh.Signer) (ssh.Signer, string) {
	certPath := strings.TrimSuffix(keyPath, ".pub") + "-cert.pub"
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, ""
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", certPath, err)
	}
	cert, ok := pub.(*ssh.Certificate)
	if !ok {
		return nil, fmt.Sprintf("%s: not an OpenSSH certificate", certPath)
	}
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", certPath, err)
	}
	return certSigner, ""
}
