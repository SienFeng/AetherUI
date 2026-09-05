package service

import (
	"os"
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

func TestIPRuleResolveDomainDefaultsToOff(t *testing.T) {
	setupDB(t)
	all, err := (&SettingService{}).GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if all.IPRuleResolveDomain != 0 {
		t.Errorf("IPRuleResolveDomain = %d, want 0", all.IPRuleResolveDomain)
	}
}

func TestCheckValidRejectsOutOfRangeIPRuleResolveDomain(t *testing.T) {
	all := validBaseSetting()
	all.IPRuleResolveDomain = 2
	if err := all.CheckValid(); err == nil {
		t.Error("expected error for a value other than 0/1")
	}
}

func TestDNSServersDefaultsToEmpty(t *testing.T) {
	setupDB(t)
	all, err := (&SettingService{}).GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if all.DNSServers != "" {
		t.Errorf("DNSServers = %q, want empty", all.DNSServers)
	}
}

// 这份名单与 app/dns/nameserver.go:53-76 的分派表一一对应，都在真实 xray
// （bin/xray-darwin-arm64，26.7.28）上 run -test 过。
// 每个 +local 形式单列一行：它们与不带 +local 的兄弟共享前缀的前几个字符，
// 用例在这里守住「前缀匹配没有互相遮蔽」。
func TestCheckValidAcceptsSupportedDNSForms(t *testing.T) {
	for _, form := range []string{
		"8.8.8.8",
		"2001:4860:4860::8888",
		"localhost",
		"https://8.8.8.8/dns-query",
		"h2c://223.5.5.5/dns-query",
		"https+local://8.8.8.8/dns-query",
		"h2c+local://223.5.5.5/dns-query",
		"quic+local://8.8.8.8",
		"tcp://223.5.5.5",
		"tcp+local://223.5.5.5",
	} {
		t.Run(form, func(t *testing.T) {
			all := validBaseSetting()
			all.DNSServers = form
			if err := all.CheckValid(); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// 这些写法 xray 全都不认识，而两种失败在面板上都看不见——见
// entity.dnsServerSchemes 的注释。IP:端口 会让 xray 直接拒绝启动
// （实测 exit 23），其余三个静默退化成连不上的 UDP 解析器。
func TestCheckValidRejectsFormsXrayDoesNotSupport(t *testing.T) {
	for _, form := range []string{
		"1.1.1.1:53",
		"[2001:4860:4860::8888]:53",
		"udp://223.5.5.5",
		"tls://8.8.8.8",
		"quic://8.8.8.8",
	} {
		t.Run(form, func(t *testing.T) {
			all := validBaseSetting()
			all.DNSServers = form
			if err := all.CheckValid(); err == nil {
				t.Errorf("%q 应被拒绝：xray 的 dns.servers 不认识这种写法", form)
			}
		})
	}
}

func TestCheckValidRejectsBareDomainDNS(t *testing.T) {
	all := validBaseSetting()
	all.DNSServers = "dns.google"
	if err := all.CheckValid(); err == nil {
		t.Error("expected error for a bare domain: xray needs a scheme or an IP")
	}
}

func TestCheckValidRejectsSchemeWithoutHost(t *testing.T) {
	all := validBaseSetting()
	all.DNSServers = "https://"
	if err := all.CheckValid(); err == nil {
		t.Error("expected error for a scheme with no host")
	}
}

// 空值必须放行：这是「不启用」的正常状态。
func TestCheckValidAcceptsEmptyDNSServers(t *testing.T) {
	all := validBaseSetting()
	all.DNSServers = ""
	if err := all.CheckValid(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// 设计 §8.6 要求保存设置时也过一遍真实 xray。这里用 tcp://8.8.8.8:99999：
// 语法上无懈可击（CheckValid 只认前缀与主机名非空），实测 xray 拒绝启动
// （exit 23，"invalid port range: 99999"）。没有这一步，管理员看到的是
// 「保存成功」+ 面板首页 running + 全员断网——dns 在 hot_diff 的 static
// 名单里，保存必然触发整进程重启，而 Process.Start() 从不回传启动失败。
func TestUpdateAllSettingRejectsDNSServerXrayWontAccept(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	s := SettingService{}
	all := validBaseSetting()
	all.DNSServers = "tcp://8.8.8.8:99999"
	if err := s.UpdateAllSetting(all); err == nil {
		t.Fatal("xray 拒绝的 dns 服务器地址应当在保存时就被顶回来")
	}
	// 校验在落库之前，被拒的值一个字节都不该写进去。
	if v, err := s.GetDNSServers(); err != nil || v != "" {
		t.Errorf("被拒绝的值不该落库，实际 %q err=%v", v, err)
	}
}

// fail open 的边界一条都不能收紧：xray 二进制不存在时照常放行。
// 校验器自身的故障绝不能变成「管理员改不了设置」。
//
// 用切换工作目录来制造「二进制不存在」：xray.GetBinaryPath() 是相对路径，
// 没有别的注入点。这是进程级副作用，本包测试串行执行，用 t.Cleanup 切回。
func TestUpdateAllSettingFailsOpenWithoutXrayBinary(t *testing.T) {
	setupDB(t)
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	s := SettingService{}
	all := validBaseSetting()
	all.DNSServers = "tcp://8.8.8.8:99999"
	if err := s.UpdateAllSetting(all); err != nil {
		t.Fatalf("没有 xray 二进制时应当放行，实际被拒: %v", err)
	}
}

// 无关设置项的保存不该 exec 一次真实 xray：那是每次保存平白多出的一秒延迟。
func TestSettingsAffectXrayConfigOnlyForRelevantKeys(t *testing.T) {
	setupDB(t)
	all := validBaseSetting()
	all.WebPort = 12345
	if settingsAffectXrayConfig(all) {
		t.Error("只改了端口，不该触发 xray 校验")
	}
	all.DNSServers = "8.8.8.8"
	if !settingsAffectXrayConfig(all) {
		t.Error("改了 dnsServers 必须触发 xray 校验")
	}
	all.DNSServers = ""
	all.IPRuleResolveDomain = 1
	if !settingsAffectXrayConfig(all) {
		t.Error("改了 ipRuleResolveDomain 必须触发 xray 校验")
	}
}
