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
//
// 断言必须打到 settings.domainStrategy 这一层，不是 outbound 顶层——
// freedom 的 domainStrategy 是 infra/conf.FreedomConfig（settings）的字段，
// 不是 infra/conf.OutboundDetourConfig（outbound 本身）的字段。写在顶层
// 会被 xray 的 infra/conf 静默丢弃（它从不对未知字段报错），这条断言若
// 只查顶层，实现写错层级时测试照样会通过。
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
	settings, ok := first["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing or not an object: %v", first)
	}
	if settings["domainStrategy"] != "UseIP" {
		t.Errorf("settings.domainStrategy = %v, want UseIP", settings["domainStrategy"])
	}
}

// 管理员改过模板、首位不是 freedom 时不动它。只有 freedom 认 domainStrategy。
//
// 断言的是 settings 整体未被改动，而不是「顶层没有 domainStrategy」——
// 后者在修好层级错误之后恒为真（实现从未写过顶层），会把这条测试变成
// 测不出任何回归的假阳性。
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
	settings, _ := first["settings"].(map[string]any)
	if len(settings) != 0 {
		t.Errorf("settings must stay untouched for a non-freedom outbound: %v", settings)
	}
}

// e2e：这条是本轮修复补的——只测 DNSInjector.Inject 本身测不出「写在了
// xray 不认的层级」这类问题，因为 DNSInjector 自己的测试只关心它做了什么，
// 不关心 xray 的 infra/conf 认不认这个字段（顶层 outbound 对象没有
// domainStrategy 这一项，但 map[string]any 反序列化不会拒绝多余的键）。
// 走 XrayService.GetXrayConfig() 才能验证最终产物里键出现在正确的位置。
//
// 这条同时守住调用顺序不变量：若 dnsInjector.Inject 被挪到
// routingInjector.Inject 之前，routingInjector 的 buildOutbounds 会用模板
// 重建整个 outbounds 数组，这里加的键会被悄悄冲掉，而 DNSInjector 自己的
// 单元测试不会发现——它们从不经过 RoutingInjector。
func TestDNSInjectorSetsFreedomDomainStrategyThroughGetXrayConfig(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	first := decodeOutbounds(t, cfg)[0]
	if first["protocol"] != "freedom" {
		t.Fatalf("first outbound protocol = %v, want freedom", first["protocol"])
	}
	settings, ok := first["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing or not an object: %v", first)
	}
	if settings["domainStrategy"] != "UseIP" {
		t.Errorf("settings.domainStrategy = %v, want UseIP", settings["domainStrategy"])
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
