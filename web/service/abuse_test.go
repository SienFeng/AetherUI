package service

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"a-ui/database/model"
)

func abuseInbound(id, port int, protocol string, enable bool) *model.Inbound {
	return &model.Inbound{Id: id, Port: port, Protocol: model.Protocol(protocol), Tag: "inbound-" + itoaS(port), Enable: enable}
}

// 25 端口与机房来源两条拦截规则：只作用于启用的、非 dokodemo-door 的入站（端口转发可能本来就来自服务器、
// 也可能本来就是转发邮件端口，那是管理员的明确配置）。
func TestAbuseRules(t *testing.T) {
	inbounds := []*model.Inbound{
		abuseInbound(2, 20002, "vless", true),
		abuseInbound(1, 20001, "vmess", true),
		abuseInbound(3, 20003, "dokodemo-door", true),
		abuseInbound(4, 20004, "trojan", false),
	}
	rules := abuseRules(abuseRuleSet{Inbounds: inbounds, BlockSmtp: true, IDCCode: "IDCC12345678"})
	if len(rules) != 2 {
		t.Fatalf("规则数 = %d，期望 2: %v", len(rules), rules)
	}
	smtp := rules[0].(map[string]any)
	if smtp["port"] != "25" || smtp["network"] != "tcp" || smtp["outboundTag"] != model.BlockOutboundTag {
		t.Errorf("25 端口规则 = %v", smtp)
	}
	wantTags := []string{"inbound-20001", "inbound-20002"}
	if !reflect.DeepEqual(smtp["inboundTag"], wantTags) {
		t.Errorf("25 端口规则的入站 = %v，期望按 id 排序、不含 dokodemo 与停用的: %v", smtp["inboundTag"], wantTags)
	}
	idc := rules[1].(map[string]any)
	if !reflect.DeepEqual(idc["source"], []string{"ext:" + idcDatRef + ":IDCC12345678"}) || idc["outboundTag"] != model.BlockOutboundTag ||
		!reflect.DeepEqual(idc["inboundTag"], wantTags) {
		t.Errorf("机房来源规则 = %v", idc)
	}
	if got := abuseRules(abuseRuleSet{Inbounds: inbounds}); len(got) != 0 {
		t.Errorf("开关都关着时不该有规则: %v", got)
	}
	if got := abuseRules(abuseRuleSet{Inbounds: []*model.Inbound{inbounds[2]}, BlockSmtp: true}); len(got) != 0 {
		t.Errorf("只有 dokodemo 入站时不该生成规则（空的 inboundTag 会匹配所有入站）: %v", got)
	}
}

// 打开「允许服务器来源」的入站只从机房规则里拿掉，25 端口照拦；入站全都豁免时机房规则整条不发——
// 空的 inboundTag 在 xray 里等于所有入站（含 api），发出去就是反过来把所有入站的机房来源都拦了。
func TestAbuseRulesSkipIDCExemptInbounds(t *testing.T) {
	relay := abuseInbound(2, 20002, "vless", true)
	relay.AllowServerSources = true
	inbounds := []*model.Inbound{abuseInbound(1, 20001, "vmess", true), relay}
	rules := abuseRules(abuseRuleSet{Inbounds: inbounds, BlockSmtp: true, IDCCode: "IDCC12345678"})
	if len(rules) != 2 {
		t.Fatalf("规则数 = %d，期望 2: %v", len(rules), rules)
	}
	if got := rules[0].(map[string]any)["inboundTag"]; !reflect.DeepEqual(got, []string{"inbound-20001", "inbound-20002"}) {
		t.Errorf("25 端口规则的入站 = %v，豁免机房的入站仍要拦 25 端口", got)
	}
	if got := rules[1].(map[string]any)["inboundTag"]; !reflect.DeepEqual(got, []string{"inbound-20001"}) {
		t.Errorf("机房规则的入站 = %v，期望不含允许服务器来源的入站", got)
	}

	rules = abuseRules(abuseRuleSet{Inbounds: []*model.Inbound{relay}, BlockSmtp: true, IDCCode: "IDCC12345678"})
	if len(rules) != 1 || rules[0].(map[string]any)["ruleTag"] != "a-ui-smtp" {
		t.Errorf("唯一的入站豁免机房时只该有 25 端口规则（机房规则的 inboundTag 为空会匹配所有入站）: %v", rules)
	}
	if got := abuseRules(abuseRuleSet{Inbounds: []*model.Inbound{relay}, IDCCode: "IDCC12345678"}); len(got) != 0 {
		t.Errorf("只开机房拦截、入站全都豁免时不该有规则: %v", got)
	}
}

// 生成规则、并发判定、自动出差剔除三处共用的判定。
func TestIDCExempt(t *testing.T) {
	relay := abuseInbound(2, 20002, "vmess", true)
	relay.AllowServerSources = true
	cases := []struct {
		in   *model.Inbound
		want bool
	}{
		{abuseInbound(1, 20001, "vmess", true), false},
		{relay, true},
		{abuseInbound(3, 20003, "dokodemo-door", true), true},
		{abuseInbound(4, 20004, "Dokodemo-Door", true), true},
	}
	for _, c := range cases {
		if got := idcExempt(c.in); got != c.want {
			t.Errorf("idcExempt(%s, allow=%v) = %v，期望 %v", c.in.Protocol, c.in.AllowServerSources, got, c.want)
		}
	}
}

// 关了嗅探的入站：打开只用于分流的嗅探（BT 与域名规则才看得见，连接目标不变）；开着但没识别 QUIC 的：
// 补上 quic（否则 HTTP/3 绕过域名规则）。dokodemo-door 等不支持嗅探的协议不动。
func TestForcedSniffing(t *testing.T) {
	cases := []struct {
		name, protocol, in string
		want               map[string]any
	}{
		{"关着", "vless", `{"enabled":false,"destOverride":["http","tls"]}`,
			map[string]any{"enabled": true, "destOverride": []any{"http", "tls", "quic"}, "routeOnly": true}},
		{"空", "trojan", `{}`,
			map[string]any{"enabled": true, "destOverride": []any{"http", "tls", "quic"}, "routeOnly": true}},
		{"开着缺 quic", "vmess", `{"enabled":true,"destOverride":["http","tls"]}`,
			map[string]any{"enabled": true, "destOverride": []any{"http", "tls", "quic"}}},
		{"开着已有 quic", "socks", `{"enabled":true,"destOverride":["http","tls","quic"],"metadataOnly":false}`,
			map[string]any{"enabled": true, "destOverride": []any{"http", "tls", "quic"}, "metadataOnly": false}},
	}
	for _, c := range cases {
		var got map[string]any
		if err := json.Unmarshal([]byte(forcedSniffing(c.protocol, c.in)), &got); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v，期望 %v", c.name, got, c.want)
		}
	}
	if got := forcedSniffing("dokodemo-door", `{}`); got != `{}` {
		t.Errorf("dokodemo-door 不该动: %s", got)
	}
	// 生成的配置必须逐字节确定：同一输入两次结果一样。
	a, b := forcedSniffing("vless", `{"enabled":false}`), forcedSniffing("vless", `{"enabled":false}`)
	if a != b {
		t.Errorf("结果不确定: %s / %s", a, b)
	}
}

// 机房网段写进单独的 dat，代码按内容寻址；换了内容之后最近的旧代码要留着，正在运行的配置还在引用它。
func TestEnsureIDCDatKeepsRecentCodes(t *testing.T) {
	resetIDCState(t)
	t.Cleanup(func() { _ = os.Remove(idcDatPath) })
	first, err := ensureIDCDat([]string{"192.0.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureIDCDat([]string{"192.0.2.0/24", "198.51.100.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("内容变了代码却没变：xray 按 file:code 缓存，会继续用旧网段")
	}
	entries, _, err := readGeoDat(idcDatPath)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, e := range entries {
		codes[e.Tag] = true
	}
	if !codes[first] || !codes[second] {
		t.Errorf("dat 里的代码 = %v，期望新旧两份都在", codes)
	}
}

// disableAbuseRules 关掉 25 端口与机房来源两条拦截：只给那些逐条数「其他功能生成了几条规则」的测试用。
func disableAbuseRules(t *testing.T) {
	t.Helper()
	s := SettingService{}
	for _, key := range []string{"blockSmtp", "blockIdcSources"} {
		if err := s.saveSetting(key, "0"); err != nil {
			t.Fatal(err)
		}
	}
}

// 生成的防滥用配置要被真实的 xray 接受：25 端口规则、引用 a-ui-idc.dat 的来源规则、只用于分流的嗅探。
// xray 不认的字段或引用不存在的 dat 代码会让整个核心起不来——这是全员断网。
func TestAbuseConfigAcceptedByXray(t *testing.T) {
	requireXrayBinary(t)
	resetIDCState(t)
	t.Cleanup(func() { _ = os.Remove(idcDatPath) })
	code, err := ensureIDCDat([]string{"192.0.2.0/24", "198.51.100.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	in := abuseInbound(1, 20101, "vless", true)
	var sniffing any
	if err := json.Unmarshal([]byte(forcedSniffing("vless", `{"enabled":false}`)), &sniffing); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"inbounds": []any{map[string]any{
			"tag": in.Tag, "port": in.Port, "listen": "127.0.0.1", "protocol": "vless",
			"settings": map[string]any{"clients": []any{map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811"}}, "decryption": "none"},
			"sniffing": sniffing,
		}},
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "tag": "direct"},
			map[string]any{"protocol": "blackhole", "tag": model.BlockOutboundTag},
		},
		"routing": map[string]any{"rules": abuseRules(abuseRuleSet{Inbounds: []*model.Inbound{in}, BlockSmtp: true, IDCCode: code})},
	}
	if err := runXrayTest(cfg); err != nil {
		t.Fatalf("xray 不接受防滥用配置: %v", err)
	}
}

// 机房网段的多数票扫描要走一遍全部源的全部网段，只在 IP 库真的换了时才重算。IPDBService.DB() 每次都
// 新建一个 Multi 包装，按包装的指针做缓存键的话缓存永远不命中：每次生成配置、每 30 秒的并发拦截重建、
// 每分钟的自动出差判定都会白扫一遍。
func TestIDCSnapshotIsCachedAcrossDBCalls(t *testing.T) {
	resetIDCState(t)
	t.Cleanup(func() { resetIDCState(t) })
	s := ipdbServiceWithSources(t, buildISPTestDB(t, "阿里云"), buildISPTestDB(t, "阿里云"))

	cidrs1, set1 := idcSnapshot(s.DB())
	if len(cidrs1) == 0 || set1 == nil {
		t.Fatalf("两个源都说是阿里云，应当算出机房网段，实际 %v", cidrs1)
	}
	if _, set2 := idcSnapshot(s.DB()); set2 != set1 {
		t.Error("IP 库没换，第二次取机房网段却重算了一遍")
	}

	s.setDB("alt", buildISPTestDB(t, "电信"))
	if cidrs3, set3 := idcSnapshot(s.DB()); set3 == set1 || len(cidrs3) != 0 {
		t.Errorf("换了一个源的库之后应当重算（一票机房一票电信，不再是机房），实际 %v", cidrs3)
	}
}

// 「补全流量识别」开着时，生成配置会给关了嗅探的入站打开只用于分流的嗅探（forcedSniffing），核心拿得到
// 域名、域名规则照常命中。流量走向验证判断「核心拿不拿得到域名」必须用同一个口径，否则对这类入站剔掉
// 域名样例、报「无法验证」，规则列表还标「永远不会命中」，把管理员推去改一条好好的规则。
func TestSniffingBlindFollowsForcedSniffing(t *testing.T) {
	off := &model.Inbound{Protocol: model.VLESS, Sniffing: `{"enabled":false}`}
	if !sniffingBlind(off, false) {
		t.Error("没开补全流量识别时，关了嗅探的 vless 入站核心拿不到域名")
	}
	if sniffingBlind(off, true) {
		t.Error("开了补全流量识别，生成配置给这个入站打开了嗅探，不该再判成拿不到域名")
	}
	empty := &model.Inbound{Protocol: model.VLESS}
	if sniffingBlind(empty, true) {
		t.Error("库里没写嗅探的入站同样会被补全")
	}
	// 嗅探开着但只识别 quic：补全只加 quic，http/tls 仍然没有，核心照样拿不到 TCP 连接的域名。
	quicOnly := &model.Inbound{Protocol: model.VLESS, Sniffing: `{"enabled":true,"destOverride":["quic"]}`}
	if !sniffingBlind(quicOnly, true) {
		t.Error("只识别 quic 的入站补全后仍没有 http/tls，应当仍判成拿不到域名")
	}
	// dokodemo-door 不在补全的协议范围里。
	dk := &model.Inbound{Protocol: model.Protocol("dokodemo-door"), Sniffing: `{"enabled":false}`}
	if !sniffingBlind(dk, true) {
		t.Error("dokodemo-door 不补全嗅探，应当仍判成拿不到域名")
	}
}

// 机房 dat 写不进去时核心里没有机房规则，机房来源真的连得进来：并发判定与自动出差剔除不能再把它们当成「被拦的」，
// 否则这段时间里从云服务器来的共享者不占并发名额。dat 写成功之后照常当成被拦的。
func TestIDCBlockedSetFollowsWhetherTheRuleWasEmitted(t *testing.T) {
	resetIDCState(t)
	t.Cleanup(func() { resetIDCState(t) })
	setupDB(t)
	s := ipdbServiceWithSources(t, buildISPTestDB(t, "阿里云"), buildISPTestDB(t, "阿里云"))
	settings := &SettingService{}
	if idcBlockedSet(settings, &s) == nil {
		t.Fatal("拦截机房来源默认打开，应当给出机房网段")
	}
	noteIDCDatResult(errors.New("磁盘满"))
	if idcBlockedSet(settings, &s) != nil {
		t.Error("机房 dat 写失败、核心没有机房规则时，仍把机房来源当成被拦的")
	}
	noteIDCDatResult(nil)
	if idcBlockedSet(settings, &s) == nil {
		t.Error("dat 写成功之后应当恢复")
	}
}
