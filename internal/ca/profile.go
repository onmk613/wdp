package ca

import (
	"crypto/x509"
	"fmt"
	"strings"
)

// Profile 是证书用途档案
type Profile string

const (
	// ProfileServer 服务端证书（ServerAuth，wdp agent 的默认用途）。
	ProfileServer Profile = "server"
	// ProfileClient 客户端证书（ClientAuth，控制端/用户身份用）。
	ProfileClient Profile = "client"
	// ProfilePeer 双向证书（ServerAuth+ClientAuth，双向 mTLS
	// 的 peer 节点证书：同一张证书既是服务端又是客户端）。
	ProfilePeer Profile = "peer"
)

// ekus 返回档案对应的扩展密钥用途。
func (p Profile) ekus() []x509.ExtKeyUsage {
	switch p {
	case ProfileClient:
		return []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	case ProfilePeer:
		return []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	default: // server / 空
		return []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
}

// NormalizeProfile 归一化档案名（空 = server；非法报错）。
func NormalizeProfile(p string) (Profile, error) {
	switch Profile(strings.ToLower(strings.TrimSpace(p))) {
	case "", ProfileServer:
		return ProfileServer, nil
	case ProfileClient:
		return ProfileClient, nil
	case ProfilePeer:
		return ProfilePeer, nil
	}
	return "", fmt.Errorf("unknown profile %q (server|client|peer)", p)
}
