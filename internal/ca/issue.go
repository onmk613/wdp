package ca

import "fmt"

// IssueOptions 是签发参数。CACertPath/CAKeyPath 是**根 CA（签发者）**的
// 证书与私钥路径——用它签出新证书，不是产物；为空时取 <Dir>/ca.crt|ca.key，
// 可显式指定以使用自制根 CA（openssl 等）。
type IssueOptions struct {
	Dir        string   // 叶子证书输出目录
	CACertPath string   // CA 证书路径（空 = <Dir>/ca.crt）
	CAKeyPath  string   // CA 私钥路径（空 = <Dir>/ca.key）
	SANs       []string // 附加 SAN，自动识别：IP → IP SAN；uri:… → URI；email:… → Email；其余 → DNS
	Profile    Profile  // 用途档案（空 = server）
	Subject    Subject  // 主题（CN 固定取 name 参数；O/OU/C/ST/L 可覆盖，O 缺省 wdp）
	Days       int      // 有效期天数：>0 显式指定；<=0 自动档（按 CA 剩余寿命分段，见 autoLeafNotAfter）；一律不超 CA 到期
	Key        KeySpec  // 叶子私钥规格（默认 ed25519）
}

// Issue 用根 CA 签发证书。职责划分：name 只决定产物文件名
// （<dir>/<name>.crt/.key）与 CN 缺省值（--cn/Subject.CN 可覆盖 CN，
// 文件名始终跟 name）；**全部 SAN 经 sans 显式给出**（自动识别
// DNS/IP/URI/Email）。server/peer 档案零 SAN 直接拒绝——现代 TLS
// 校验只认 SAN 不认 CN，无 SAN 的服务端证书过不了主机名校验。
func Issue(o IssueOptions, name string) (string, string, string, error) {
	profile, err := NormalizeProfile(string(o.Profile))
	if err != nil {
		return "", "", "", err
	}
	// name 为空时以档案名为证书名（文件名与 CN 缺省）：
	// wdp ca issue --san web1 --profile peer → peer.crt/.key, CN=peer
	if name == "" {
		name = string(profile)
	}
	if profile != ProfileClient && len(o.SANs) == 0 {
		return "", "", "", fmt.Errorf(
			"profile %q requires at least one SAN via --san (modern TLS verifies SAN only, CN is not checked)", profile)
	}
	key, err := o.Key.Generate()
	if err != nil {
		return "", "", "", err
	}
	tpl, err := leafTemplate(name, o.SANs, profile, o.Days)
	if err != nil {
		return "", "", "", err
	}
	cn := name
	if o.Subject.CN != "" {
		cn = o.Subject.CN
	}
	tpl.Subject = o.Subject.fillDefaults().name(cn)
	return signAndWrite(o.Dir, o.CACertPath, o.CAKeyPath, name, tpl, key)
}
