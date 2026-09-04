package service

import (
	"testing"
)

func TestDefaultInboundSettingsRoundTrip(t *testing.T) {
	setupDB(t)
	s := SettingService{}

	if v, err := s.GetDefaultDomain(); err != nil || v != "" {
		t.Fatalf("默认值应为空串，得到 %q err=%v", v, err)
	}
	if err := s.SetDefaultDomain("example.com"); err != nil {
		t.Fatalf("SetDefaultDomain: %v", err)
	}
	if err := s.SetDefaultCertFile("/root/cert/fullchain.cer"); err != nil {
		t.Fatalf("SetDefaultCertFile: %v", err)
	}
	if err := s.SetDefaultKeyFile("/root/cert/example.com.key"); err != nil {
		t.Fatalf("SetDefaultKeyFile: %v", err)
	}

	if v, _ := s.GetDefaultDomain(); v != "example.com" {
		t.Fatalf("domain 期望 example.com，实际 %q", v)
	}
	if v, _ := s.GetDefaultCertFile(); v != "/root/cert/fullchain.cer" {
		t.Fatalf("certFile 实际 %q", v)
	}
	if v, _ := s.GetDefaultKeyFile(); v != "/root/cert/example.com.key" {
		t.Fatalf("keyFile 实际 %q", v)
	}
}

// 这三个字段只是「新建入站时的默认填充值」，面板自己不使用它们。
// 若照抄 WebCertFile 的 tls.LoadX509KeyPair 校验，证书尚未签发时保存
// 设置页会整个失败——包括端口、时区等无关字段。
//
// 用 validBaseSetting()（见 setting_baseline_test.go）而不是手写一份基线：
// CheckValid 逐条串行校验，手写基线漏掉任何一条既有规则（比如「IP 归属地
// 库源地址至少留一个」）都会让测试因为与本测试无关的字段报错——这个坑
// validBaseSetting 的注释里说已经踩过三次了，不需要在这里踩第四次。
func TestCheckValidDoesNotLoadDefaultCertPair(t *testing.T) {
	s := validBaseSetting()
	s.DefaultDomain = "example.com"
	s.DefaultCertFile = "/root/cert/does-not-exist.cer"
	s.DefaultKeyFile = "/root/cert/does-not-exist.key"
	if err := s.CheckValid(); err != nil {
		t.Fatalf("默认证书路径不存在时不应报错，实际: %v", err)
	}
}

func TestCheckValidRejectsRelativeDefaultCertPath(t *testing.T) {
	s := validBaseSetting()
	s.DefaultCertFile = "relative/path.cer"
	if err := s.CheckValid(); err == nil {
		t.Fatal("相对路径应被拒绝")
	}
}

// 校验逻辑对 DefaultCertFile 与 DefaultKeyFile 是同一个循环，单独补一条
// key file 的用例：防止将来有人改动时只顾着其中一个字段。
func TestCheckValidRejectsRelativeDefaultKeyPath(t *testing.T) {
	s := validBaseSetting()
	s.DefaultKeyFile = "relative/path.key"
	if err := s.CheckValid(); err == nil {
		t.Fatal("相对路径应被拒绝")
	}
}
