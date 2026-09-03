package model

import "testing"

// 实测确认（Xray 26.7.28）：纯 IPv4 的允许集配上 ext:file:!TAG 取反，
// 遇到 IPv6 来源会 **放行**——匹配器不会拿 v6 地址去比对一个没有 v6 条目
// 的集合。所以启用地区限制的入站必须只监听 IPv4，让客户端根本连不上来。
func TestGenXrayInboundConfigForcesIPv4ListenWhenRegionsSet(t *testing.T) {
	in := &Inbound{
		Port: 10011, Protocol: VLESS, Tag: "inbound-10011",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
		Regions: `["江苏省"]`,
	}
	cfg := in.GenXrayInboundConfig()
	if got := string(cfg.Listen); got != `"0.0.0.0"` {
		t.Errorf("listen = %s，配了地区限制且未指定监听地址时应当限定为 0.0.0.0", got)
	}
}

func TestGenXrayInboundConfigKeepsEmptyListenWithoutRegions(t *testing.T) {
	for _, regions := range []string{"", "null", "[]"} {
		in := &Inbound{
			Port: 10011, Protocol: VLESS, Tag: "inbound-10011",
			Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
			Regions: regions,
		}
		cfg := in.GenXrayInboundConfig()
		// 没有地区限制时必须保持原样：擅自改成 0.0.0.0 会让现有部署的
		// IPv6 客户端全部断开，而且配置字节变化会触发一次无谓的重启。
		if len(cfg.Listen) != 0 {
			t.Errorf("regions=%q 时 listen = %s，期望留空", regions, cfg.Listen)
		}
	}
}

func TestGenXrayInboundConfigRespectsExplicitListen(t *testing.T) {
	in := &Inbound{
		Listen: "10.0.0.5", Port: 10011, Protocol: VLESS, Tag: "inbound-10011",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
		Regions: `["江苏省"]`,
	}
	cfg := in.GenXrayInboundConfig()
	// 管理员显式指定的监听地址不覆盖，那是他的选择；IPv6 由另一条
	// ::/0 的拒绝规则兜底。
	if got := string(cfg.Listen); got != `"10.0.0.5"` {
		t.Errorf("listen = %s，期望保留管理员指定的 10.0.0.5", got)
	}
}
