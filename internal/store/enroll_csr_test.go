package store

// LockEnrollCSRKey 的回归：token 锁定到首个 CSR 公钥——同钥幂等、
// 异钥拒绝（token 泄露后换钥匙冒名的检测点）。

import (
	"testing"
	"time"
)

func TestLockEnrollCSRKey(t *testing.T) {
	s := openTest(t)
	if err := s.CreateEnrollToken("tok", "h1", 7602, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.LockEnrollCSRKey("tok", "aa"); err != nil {
		t.Fatal(err)
	}
	// 同钥幂等
	if err := s.LockEnrollCSRKey("tok", "aa"); err != nil {
		t.Fatalf("同钥重试应幂等: %v", err)
	}
	// 异钥拒绝（业务错误）
	if err := s.LockEnrollCSRKey("tok", "bb"); !IsBizErr(err) {
		t.Fatalf("异钥应业务错误拒绝: %v", err)
	}
	// 锁定状态落库可查
	got, err := s.GetEnrollToken("tok")
	if err != nil || got.CSRPubkeySHA != "aa" {
		t.Fatalf("锁定状态应落库: %+v %v", got, err)
	}
	// 过期 token 不可锁定
	if err := s.CreateEnrollToken("tokold", "h2", 7602, -time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.LockEnrollCSRKey("tokold", "aa"); err == nil {
		t.Fatal("过期 token 应拒绝")
	}
}
