package web

// 控制台→agent 的连接模型回归：
//   - 台账未声明明文时按 mTLS 建连，且**按台账地址校验证书身份**
//     （曾经是"只验链不验名"：任何本 CA 签发的证书都被接受，任一台
//     agent 沦陷即可冒充任意其它主机）；
//   - 只有显式 allow_plaintext 的台账行才走明文。

import (
	"testing"

	"wdp/internal/store"
)

// TestAgentHostModelVerifiesHostname 纳管主机：TLS + ServerName=台账地址，
// 且不得出现"跳过主机名校验"的降级开关。
func TestAgentHostModelVerifiesHostname(t *testing.T) {
	s, _ := newAppServerMTLS(t, 18780)
	h := &store.Host{Name: "mtls-local", Address: "10.1.2.3", AgentPort: 7602}

	m := s.agentHostModel(h)
	if !m.TLS {
		t.Fatal("CA 就绪且未声明明文：应按 mTLS 建连")
	}
	if m.TLSSkipHostVerify || m.InsecureSkipVerify {
		t.Fatal("不得关闭主机名校验（冒充任意 agent 的入口）")
	}
	if m.TLSServerName != "10.1.2.3" {
		t.Fatalf("校验名应为台账地址: %q", m.TLSServerName)
	}

	// 带端口的地址取裸主机名（证书 SAN 从不含端口）
	m2 := s.agentHostModel(&store.Host{Name: "p", Address: "10.1.2.3:7602", AgentPort: 7602})
	if m2.TLSServerName != "10.1.2.3" {
		t.Fatalf("带端口地址应剥端口: %q", m2.TLSServerName)
	}
}

// TestAgentHostModelPlaintextOptIn 只有显式声明的主机才走明文。
func TestAgentHostModelPlaintextOptIn(t *testing.T) {
	s, _ := newAppServerMTLS(t, 18781)
	m := s.agentHostModel(&store.Host{Name: "plain", Address: "10.1.2.4", AgentPort: 7602, AllowPlaintext: true})
	if m.TLS {
		t.Fatal("显式声明明文的主机不应按 mTLS 建连")
	}
	if s.agentScheme(&store.Host{Name: "plain", Address: "10.1.2.4", AgentPort: 7602, AllowPlaintext: true}) != "http" {
		t.Fatal("显式明文主机的 scheme 应为 http")
	}
	if s.agentScheme(&store.Host{Name: "mtls", Address: "10.1.2.5", AgentPort: 7602}) != "https" {
		t.Fatal("未声明明文的主机 scheme 应为 https")
	}
	if c := s.probeClientFor(&store.Host{Name: "plain", Address: "10.1.2.4", AllowPlaintext: true}); c != nil {
		t.Fatal("显式明文主机的探活客户端应为 nil（走明文）")
	}
	if c := s.probeClientFor(&store.Host{Name: "mtls", Address: "10.1.2.5"}); c == nil {
		t.Fatal("纳管主机的探活客户端应带 mTLS")
	}
}

// TestAgentSchemeIndependentOfProbe 通道选择不得依赖探活结果：
// 台账+CA 状态决定（探活可被中间人操纵，用它决定通道等于把降级开关
// 交给攻击者）。
func TestAgentSchemeIndependentOfProbe(t *testing.T) {
	s, _ := newAppServerMTLS(t, 18782)
	// 127.0.0.1:1 必然不可达；scheme 仍必须是 https（不可达即报错，
	// 而不是"探不通就退回明文"）
	if got := s.agentScheme(&store.Host{Name: "down", Address: "127.0.0.1", AgentPort: 1}); got != "https" {
		t.Fatalf("不可达的纳管主机仍应按 https 处理: %q", got)
	}
}
