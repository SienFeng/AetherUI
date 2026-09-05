package service

import (
	"encoding/json"
	"testing"
)

// 不配置 = 一个字节都不改。升级后行为零变化靠的就是这一条。
func TestDNSInjectorNoopWhenUnset(t *testing.T) {
	setupDB(t)
	cfg := newTemplateConfig(t)
	beforeDNS := string(cfg.DNSConfig)
	beforeOut := string(cfg.OutboundConfigs)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if string(cfg.DNSConfig) != beforeDNS {
		t.Errorf("DNSConfig changed: %q -> %q", beforeDNS, cfg.DNSConfig)
	}
	if string(cfg.OutboundConfigs) != beforeOut {
		t.Errorf("OutboundConfigs changed")
	}
}

func TestDNSInjectorWritesServersAndFallback(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "https://8.8.8.8/dns-query\n1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	var dns struct {
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(cfg.DNSConfig, &dns); err != nil {
		t.Fatalf("unmarshal dns: %v", err)
	}
	want := []string{"https://8.8.8.8/dns-query", "1.1.1.1", "localhost"}
	if len(dns.Servers) != len(want) {
		t.Fatalf("servers = %v, want %v", dns.Servers, want)
	}
	for i := range want {
		if dns.Servers[i] != want[i] {
			t.Errorf("servers[%d] = %q, want %q", i, dns.Servers[i], want[i])
		}
	}
}

// 管理员自己写了 localhost 就不再补一个：重复项无害但会让界面与配置对不上。
func TestDNSInjectorDoesNotDuplicateFallback(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "localhost\n1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	var dns struct {
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(cfg.DNSConfig, &dns); err != nil {
		t.Fatalf("unmarshal dns: %v", err)
	}
	count := 0
	for _, s := range dns.Servers {
		if s == "localhost" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("localhost appears %d times in %v, want 1", count, dns.Servers)
	}
}

// 不设这一项，dns 段对直连流量完全是空转：freedom 只在
// domainStrategy.HasStrategy() 为真时才走 xray 的内置 DNS 客户端。
func TestDNSInjectorSetsFreedomDomainStrategy(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	first := decodeOutbounds(t, cfg)[0]
	if first["domainStrategy"] != "UseIP" {
		t.Errorf("domainStrategy = %v, want UseIP", first["domainStrategy"])
	}
}

// 管理员改过模板、首位不是 freedom 时不动它。只有 freedom 认 domainStrategy。
func TestDNSInjectorLeavesNonFreedomDefaultOutboundAlone(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	cfg.OutboundConfigs = []byte(`[{"protocol":"socks","tag":"custom","settings":{}}]`)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	first := decodeOutbounds(t, cfg)[0]
	if _, ok := first["domainStrategy"]; ok {
		t.Errorf("must not set domainStrategy on a non-freedom outbound: %v", first)
	}
}

// 生成必须逐字节确定，否则 Config.Equals 恒为 false，10 秒的重启 cron
// 会不停重启 xray。
func TestDNSInjectorIsDeterministic(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "https://8.8.8.8/dns-query\n1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	first := newTemplateConfig(t)
	second := newTemplateConfig(t)
	if err := (&DNSInjector{}).Inject(first); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := (&DNSInjector{}).Inject(second); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if string(first.DNSConfig) != string(second.DNSConfig) {
		t.Errorf("dns not byte-identical:\n%s\n%s", first.DNSConfig, second.DNSConfig)
	}
	if string(first.OutboundConfigs) != string(second.OutboundConfigs) {
		t.Errorf("outbounds not byte-identical")
	}
}

func TestParseDNSServersTrimsAndDedupes(t *testing.T) {
	got := ParseDNSServers("  1.1.1.1  \n\n1.1.1.1\n 8.8.8.8 \n")
	if len(got) != 2 || got[0] != "1.1.1.1" || got[1] != "8.8.8.8" {
		t.Errorf("got = %v, want [1.1.1.1 8.8.8.8]", got)
	}
}
