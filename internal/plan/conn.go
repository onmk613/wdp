package plan

import "wdp/internal/model"

// HostOf 把计划连接元数据还原为执行用主机（密钥经 env 引用恢复）。
func (c HostConn) Host(name string) *model.Host {
	h := &model.Host{
		Name:               name,
		Address:            c.Address,
		Port:               c.Port,
		User:               c.User,
		Conn:               c.Conn,
		AgentURL:           c.AgentURL,
		AgentPort:          c.AgentPort,
		TLS:                c.TLS,
		InsecureSkipVerify: c.InsecureSkipVerify,
		TLSSkipHostVerify:  c.TLSSkipHostVerify,
		TLSServerName:      c.TLSServerName,
		CAFile:             c.CAFile,
		CertFile:           c.CertFile,
		KeyFile:            c.KeyFile,
		PasswordEnv:        c.PasswordEnv,
		Password:           c.PasswordEnvRef,
		KeyPassphraseEnv:   c.KeyPassphraseEnv,
		BecomePasswordEnv:  c.BecomePasswordEnv,
		BecomePassword:     c.BecomePasswordRef,
		Vars:               map[string]any{},
	}
	if h.Address == "" {
		h.Address = name
	}
	return h
}
