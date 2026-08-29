package ca

import "crypto/x509/pkix"

// Subject 是证书主题
type Subject struct {
	CN string   // CommonName
	O  []string // Organization
	OU []string // OrganizationalUnit
	C  []string // Country（两字母代码）
	ST []string // State/Province
	L  []string // Locality（城市）
}

// fillDefaults 空字段补 wdp 缺省（O=wdp；CN 由调用方定，不再覆盖）。
func (s Subject) fillDefaults() Subject {
	if len(s.O) == 0 {
		s.O = []string{"wdp"}
	}
	return s
}

// name 转换为 pkix.Name。
func (s Subject) name(commonName string) pkix.Name {
	return pkix.Name{
		CommonName:         commonName,
		Organization:       s.O,
		OrganizationalUnit: s.OU,
		Country:            s.C,
		Province:           s.ST,
		Locality:           s.L,
	}
}
