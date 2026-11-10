package service

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// wantGuard 是默认出站 settings.finalRules 末尾应当出现的那一条。
const wantGuard = `{"action":"block","ip":["geoip:private"]}`

func finalRulesOf(t *testing.T, ob map[string]any) []string {
	t.Helper()
	settings, _ := ob["settings"].(map[string]any)
	rules, _ := settings["finalRules"].([]any)
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func newDokodemoInbound(t *testing.T, port int, address string, targetPort int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.Protocol("dokodemo-door"), Enable: true,
		Tag:            "inbound-" + strconv.Itoa(port),
		Listen:         "127.0.0.1",
		Settings:       `{"address":"` + address + `","port":` + strconv.Itoa(targetPort) + `,"network":"tcp"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Sniffing:       "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save dokodemo inbound: %v", err)
	}
	return in
}

// socks/http 入站的用户用一个主机名（localhost、解析到内网的域名）就能让默认 freedom 连到本机回环、
// 内网与云元数据——包括 xray 自己那个不鉴权的 gRPC 控制面。模板的 geoip:private 规则只对 IP 目标
// 生效，而 freedom 内置的私网拦截只覆盖 vless/vmess/trojan/shadowsocks/hysteria/wireguard 入站。
// 守卫必须落在 freedom 自己的 finalRules 上：它看的是解析之后真正要拨的地址。
func TestGeneratedDefaultFreedomBlocksPrivateDestinations(t *testing.T) {
	setupDB(t)
	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	if obs[0]["protocol"] != "freedom" {
		t.Fatalf("前提不成立：默认出站不是 freedom: %v", obs[0])
	}
	got := finalRulesOf(t, obs[0])
	if len(got) != 1 || got[0] != wantGuard {
		t.Fatalf("默认 freedom 出站的 finalRules = %v，期望 [%s]", got, wantGuard)
	}
}

// 计量出站是默认出站的副本（被计量的直连流量走它），必须带同一道守卫，否则一个被计量的
// 主机名就能绕开守卫。它们本来就是逐字节副本，守卫要在复制之前加上。
func TestMeterOutboundsInheritPrivateGuard(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32101)
	putPoolRow(t, in.Id, "example.com", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	found := false
	for _, ob := range decodeOutbounds(t, cfg) {
		tag, _ := ob["tag"].(string)
		if !model.IsMeterTag(tag) {
			continue
		}
		found = true
		if got := finalRulesOf(t, ob); len(got) != 1 || got[0] != wantGuard {
			t.Errorf("计量出站 %s 的 finalRules = %v，期望 [%s]", tag, got, wantGuard)
		}
	}
	if !found {
		t.Fatal("前提不成立：池里有一行却没生成计量出站")
	}
}

// 管理员在模板里手写的 finalRules 原样保留、守卫追加在末尾：xray 按顺序取第一条命中的，
// 管理员显式放行的网段照常放行（开头写一条 {"action":"allow"} 就是整个关掉守卫的写法）。
// 已经写了同一条守卫的不重复追加。
func TestPrivateGuardKeepsAdminFinalRulesFirst(t *testing.T) {
	cases := []struct {
		name  string
		rules string
		want  []string
	}{
		{"admin allow first", `[{"action":"allow","ip":["10.0.0.0/8"]}]`,
			[]string{`{"action":"allow","ip":["10.0.0.0/8"]}`, wantGuard}},
		{"already present", `[` + wantGuard + `]`, []string{wantGuard}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupDB(t)
			cfg := newTemplateConfig(t)
			var obs []map[string]any
			if err := json.Unmarshal(cfg.OutboundConfigs, &obs); err != nil {
				t.Fatal(err)
			}
			var rules any
			if err := json.Unmarshal([]byte(c.rules), &rules); err != nil {
				t.Fatal(err)
			}
			obs[0]["settings"] = map[string]any{"finalRules": rules}
			raw, _ := json.Marshal(obs)
			cfg.OutboundConfigs = raw
			if err := (&RoutingInjector{}).Inject(cfg); err != nil {
				t.Fatalf("Inject: %v", err)
			}
			got := finalRulesOf(t, decodeOutbounds(t, cfg)[0])
			if len(got) != len(c.want) {
				t.Fatalf("finalRules = %v，期望 %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("finalRules = %v，期望 %v", got, c.want)
				}
			}
		})
	}
}

// 管理员把默认出站换成了别的协议（比如经上游代理出网）时不加：finalRules 只有 freedom 认。
func TestPrivateGuardLeavesNonFreedomDefaultAlone(t *testing.T) {
	setupDB(t)
	cfg := newTemplateConfig(t)
	cfg.OutboundConfigs = []byte(`[{"protocol":"blackhole","settings":{}}]`)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := finalRulesOf(t, decodeOutbounds(t, cfg)[0]); len(got) != 0 {
		t.Fatalf("非 freedom 的默认出站被加了 finalRules: %v", got)
	}
}

// dokodemo-door 是管理员亲手配的端口转发（目标固定），转发到内网主机名是正当用法，守卫不能误伤：
// 它们的流量在其余规则都没命中之后走一份不带守卫的默认出站副本。这条兜底规则必须排在管理员规则
// 之后（分流/封禁照样优先）、计量规则之前（否则一个被计量的内网主机名会掉进带守卫的计量副本）。
func TestDokodemoInboundsGetUnguardedDirectRoute(t *testing.T) {
	setupMeterPoolTest(t)
	socks := newTestInbound(t, 32111)
	doko := newDokodemoInbound(t, 32112, "localhost", 8080)
	putPoolRow(t, socks.Id, "example.com", 0)

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	var direct map[string]any
	for _, ob := range obs {
		if ob["tag"] == dokodemoDirectTag {
			if direct != nil {
				t.Fatal("dokodemo 直连副本出现了两次")
			}
			direct = ob
		}
	}
	if direct == nil {
		t.Fatalf("有 dokodemo 入站却没有生成 %s 出站", dokodemoDirectTag)
	}
	if direct["protocol"] != "freedom" {
		t.Errorf("dokodemo 直连副本协议 = %v，期望 freedom", direct["protocol"])
	}
	if got := finalRulesOf(t, direct); len(got) != 0 {
		t.Errorf("dokodemo 直连副本不该带守卫: %v", got)
	}

	rules := decodeRules(t, cfg)
	catchIdx, firstMeter := -1, -1
	for i, r := range rules {
		tag, _ := r["outboundTag"].(string)
		if tag == dokodemoDirectTag {
			catchIdx = i
		}
		if model.IsMeterTag(tag) && firstMeter < 0 {
			firstMeter = i
		}
	}
	if catchIdx < 0 {
		t.Fatal("没有把 dokodemo 入站送到直连副本的规则")
	}
	if firstMeter < 0 || catchIdx > firstMeter {
		t.Fatalf("dokodemo 兜底规则（第 %d 条）必须排在计量规则（第 %d 条）之前", catchIdx, firstMeter)
	}
	for i := catchIdx + 1; i < len(rules); i++ {
		if tag, _ := rules[i]["outboundTag"].(string); !model.IsMeterTag(tag) {
			t.Fatalf("dokodemo 兜底规则之后还有非计量规则 %v：它会抢在管理员规则前面", rules[i])
		}
	}
	catch := rules[catchIdx]
	if !jsonEq(t, catch["inboundTag"], []string{doko.Tag}) {
		t.Errorf("兜底规则 inboundTag = %v，期望只含 [%s]（api 与非 dokodemo 入站都不能在里面）", catch["inboundTag"], doko.Tag)
	}
	if _, ok := catch["ip"]; ok {
		t.Error("单遍匹配下兜底规则不该带 ip 守卫")
	}
}

// 两遍匹配（ipRuleResolveDomain=1）下兜底规则必须带 ip 守卫，与计量规则同理：不带的话它在第一遍
// 就命中，模板那条 geoip:private 对 dokodemo 的域名目标再也没有第二遍的机会——比现状更松。
func TestDokodemoDirectRouteCarriesIPGuardWhenCoreResolvesDomains(t *testing.T) {
	setupDB(t)
	newDokodemoInbound(t, 32121, "localhost", 8080)
	if err := (&SettingService{}).setString("ipRuleResolveDomain", "1"); err != nil {
		t.Fatal(err)
	}
	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	for _, r := range decodeRules(t, cfg) {
		if r["outboundTag"] == dokodemoDirectTag {
			if !jsonEq(t, r["ip"], []string{"0.0.0.0/0", "::/0"}) {
				t.Fatalf("两遍匹配下兜底规则 ip = %v，期望 [0.0.0.0/0 ::/0]", r["ip"])
			}
			return
		}
	}
	t.Fatal("没有找到 dokodemo 兜底规则")
}

// 配了内置 DNS 时，副本要和默认出站一样走 UseIP：副本在 DNS 注入之后才复制，否则 dokodemo 的
// 主机名目标会悄悄绕过管理员配的解析器——正是 DNS 注入器对计量副本防着的那种空转。
func TestDokodemoDirectRouteKeepsDNSStrategy(t *testing.T) {
	setupDB(t)
	newDokodemoInbound(t, 32131, "localhost", 8080)
	if err := (&SettingService{}).setString("dnsServers", "https://1.1.1.1/dns-query"); err != nil {
		t.Fatal(err)
	}
	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	for _, ob := range decodeOutbounds(t, cfg) {
		if ob["tag"] == dokodemoDirectTag {
			settings, _ := ob["settings"].(map[string]any)
			if settings["domainStrategy"] != freedomDomainStrategy {
				t.Fatalf("副本 domainStrategy = %v，期望 %s", settings["domainStrategy"], freedomDomainStrategy)
			}
			return
		}
	}
	t.Fatal("没有找到 dokodemo 直连副本")
}

// 没有 dokodemo 入站时一个字节都不多：不生成副本、不加规则。
func TestNoDokodemoMeansNoDirectCopy(t *testing.T) {
	setupDB(t)
	newTestInbound(t, 32141)
	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	if bytes.Contains(cfg.OutboundConfigs, []byte(dokodemoDirectTag)) ||
		bytes.Contains(cfg.RouterConfig, []byte(dokodemoDirectTag)) {
		t.Fatal("没有 dokodemo 入站却生成了直连副本或兜底规则")
	}
}

// 生成必须逐字节确定，否则 Config.Equals 恒为假，10 秒的消费任务会不停重启核心。
func TestFreedomGuardGenerationIsDeterministic(t *testing.T) {
	setupDB(t)
	newTestInbound(t, 32151)
	newDokodemoInbound(t, 32152, "localhost", 8080)
	newDokodemoInbound(t, 32153, "192.0.2.10", 8443)
	xs := &XrayService{}
	a, err := xs.GetXrayConfig()
	if err != nil {
		t.Fatal(err)
	}
	b, err := xs.GetXrayConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equals(b) {
		t.Fatal("两次生成的配置不相等")
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatal("两次生成的配置字节不同")
	}
	// 升级前核心持有的是不带守卫的配置：默认出站变了，升级后的第一次下发必须走整进程重启
	// （没有换默认出站的运行时接口），之后配置稳定、不再重启。
	var obs []map[string]any
	if err := json.Unmarshal(a.OutboundConfigs, &obs); err != nil {
		t.Fatal(err)
	}
	delete(obs[0]["settings"].(map[string]any), "finalRules")
	oldOutbounds, _ := json.Marshal(obs)
	old := *a
	old.OutboundConfigs = oldOutbounds
	if _, ok := xray.ComputeHotDiff(&old, a); ok {
		t.Fatal("默认出站加了守卫却被判定为可热应用：核心会继续跑不带守卫的默认出站")
	}
}

// 一个 tag 恰好是注入器专用 tag 的节点（只可能来自导入）不能进配置：撞名会让 xray 以
// existing tag found 拒绝启动整份配置。
func TestNodeWithInjectorTagIsSkipped(t *testing.T) {
	setupDB(t)
	newDokodemoInbound(t, 32161, "localhost", 8080)
	node := &model.OutboundNode{Tag: dokodemoDirectTag, Remark: "撞名", Protocol: "socks", Enable: true,
		Config: `{"protocol":"socks","settings":{"servers":[{"address":"192.0.2.1","port":1080}]}}`}
	if err := database.GetDB().Create(node).Error; err != nil {
		t.Fatal(err)
	}
	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	n := 0
	for _, ob := range decodeOutbounds(t, cfg) {
		if ob["tag"] == dokodemoDirectTag {
			n++
			if ob["protocol"] != "freedom" {
				t.Errorf("占用 %s 的是节点而不是直连副本: %v", dokodemoDirectTag, ob)
			}
		}
	}
	if n != 1 {
		t.Fatalf("tag %s 出现了 %d 次，期望恰好 1 次", dokodemoDirectTag, n)
	}
}
