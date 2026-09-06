package service

import (
	"encoding/json"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// 建库与写池行都复用 meter_pool_test.go 里的 setupMeterPoolTest / putPoolRow，
// 出站与规则的解码复用 routing_inject_test.go 里的 decodeOutbounds / decodeRules，
// 不再重复一份。
func TestInjectWithEmptyPoolChangesNothing(t *testing.T) {
	setupMeterPoolTest(t)
	newTestInbound(t, 32001)

	withPool := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(withPool); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, withPool)
	// 池为空 = 一个计量出站都不生成，最后一个仍然是黑洞出站。
	if obs[len(obs)-1]["tag"] != model.BlockOutboundTag {
		t.Errorf("最后一个出站 = %v，期望 %s", obs[len(obs)-1]["tag"], model.BlockOutboundTag)
	}
	for _, ob := range obs {
		if tag, _ := ob["tag"].(string); model.IsMeterTag(tag) {
			t.Errorf("池为空却生成了计量出站 %q", tag)
		}
	}
}

func TestInjectAppendsMeterOutboundsAsDefaultCopies(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32002)
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	putPoolRow(t, in.Id, "example.com", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)

	// 计量出站追加在最末，顺序按 (inboundId asc, domain asc)。
	n := len(obs)
	wantTags := []string{
		model.MeterTag(in.Id, "doubleclick.net"),
		model.MeterTag(in.Id, "example.com"),
	}
	for i, want := range wantTags {
		got := obs[n-len(wantTags)+i]["tag"]
		if got != want {
			t.Errorf("倒数第 %d 个出站 tag = %v，期望 %q", len(wantTags)-i, got, want)
		}
	}
	// 黑洞出站仍在计量出站之前。
	if obs[n-len(wantTags)-1]["tag"] != model.BlockOutboundTag {
		t.Errorf("计量出站之前应当是黑洞出站，实际是 %v", obs[n-len(wantTags)-1]["tag"])
	}
	// 深拷贝的可观测判据：浅拷贝会让所有克隆共享同一个 map，于是默认出站
	// 会被写上最后一个计量 tag，而 diffOutbounds 要求首位逐字节不变——
	// 每次换池都会退化成整进程重启。
	if firstTag, _ := obs[0]["tag"].(string); model.IsMeterTag(firstTag) {
		t.Fatalf("默认出站的 tag 变成了 %q，说明计量出站是浅拷贝、与它共享了同一个 map", firstTag)
	}

	// 除 tag 外必须与默认出站逐字节相同——它就是默认出站的副本，
	// 换个 tag 只是为了让 xray 单独计数，转发行为一模一样。
	stripTag := func(ob map[string]any) string {
		clone := map[string]any{}
		for k, v := range ob {
			if k != "tag" {
				clone[k] = v
			}
		}
		encoded, err := json.Marshal(clone)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(encoded)
	}
	want := stripTag(obs[0])
	for i := n - len(wantTags); i < n; i++ {
		if got := stripTag(obs[i]); got != want {
			t.Errorf("计量出站内容 = %s，期望与默认出站相同 %s", got, want)
		}
	}
}

func TestInjectMeterRulesComeLastWithoutGuardByDefault(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32003)
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	last := rules[len(rules)-1]

	if last["outboundTag"] != model.MeterTag(in.Id, "doubleclick.net") {
		t.Fatalf("最后一条规则 = %+v，期望是计量规则", last)
	}
	if got := last["domain"]; !jsonEq(t, got, []string{"domain:doubleclick.net"}) {
		t.Errorf("domain = %v，期望 [domain:doubleclick.net]——含点的裸串在 xray 里是"+
			"子串匹配，会命中 notdoubleclick.net.evil", got)
	}
	if got := last["inboundTag"]; !jsonEq(t, got, []string{in.Tag}) {
		t.Errorf("inboundTag = %v，期望 [%s]", got, in.Tag)
	}
	// 默认（模板没写 domainStrategy、开关为 0）是单遍匹配：ip 条件规则对
	// 域名目标本来就永不命中，没有第二遍可屏蔽，纯形态是安全的；反过来
	// 带上守卫会让计量完全空转。
	if _, ok := last["ip"]; ok {
		t.Error("单遍匹配下不该带 ip 守卫，否则守卫恒假、计量完全空转")
	}
}

func TestInjectMeterRulesCarryIPGuardWhenCoreResolvesDomains(t *testing.T) {
	// 三种「核心会解析域名」的形态都要带守卫。不带的话计量规则会在第一遍
	// 命中并吃掉第二遍，模板自带的 geoip:private 与管理员所有 CIDR 规则对
	// 池内域名静默失效（真实 xray 上实测复现，spec §6.1 用例 A）。
	cases := []struct {
		name          string
		resolveDomain bool
		template      string
		wantGuard     bool
	}{
		{"开关打开 → IPIfNonMatch", true, "", true},
		{"模板手写 IPOnDemand", false, "IPOnDemand", true},
		{"模板手写小写 ipifnonmatch", false, "ipifnonmatch", true},
		{"模板手写无法识别的值", false, "IPSometimes", false},
		{"模板手写 AsIs", false, "AsIs", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupMeterPoolTest(t)
			in := newTestInbound(t, 32010)
			putPoolRow(t, in.Id, "doubleclick.net", 0)
			if c.resolveDomain {
				if err := (&SettingService{}).setString("ipRuleResolveDomain", "1"); err != nil {
					t.Fatalf("setString: %v", err)
				}
			}
			cfg := newTemplateConfig(t)
			if c.template != "" {
				cfg.RouterConfig = []byte(
					`{"domainStrategy":"` + c.template + `","rules":[]}`)
			}
			if err := (&RoutingInjector{}).Inject(cfg); err != nil {
				t.Fatalf("Inject: %v", err)
			}
			rules := decodeRules(t, cfg)
			last := rules[len(rules)-1]
			ip, hasIP := last["ip"]
			if c.wantGuard {
				if !hasIP || !jsonEq(t, ip, []string{"0.0.0.0/0", "::/0"}) {
					t.Errorf("ip = %v，期望 [0.0.0.0/0 ::/0]——没有守卫，计量规则会在"+
						"第一遍命中并吃掉第二遍，IP 段规则对池内域名静默失效", ip)
				}
			} else if hasIP {
				t.Errorf("ip = %v，期望不带守卫——核心不解析域名时守卫恒假，计量完全空转", ip)
			}
		})
	}
}

func TestInjectSkipsMeterRowsOfDisabledInbounds(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32004)
	in.Enable = false
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("停用入站: %v", err)
	}
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, ob := range decodeOutbounds(t, cfg) {
		if tag, _ := ob["tag"].(string); model.IsMeterTag(tag) {
			t.Errorf("为停用入站生成了计量出站 %q", tag)
		}
	}
	for _, r := range decodeRules(t, cfg) {
		if tag, _ := r["outboundTag"].(string); model.IsMeterTag(tag) {
			t.Errorf("为停用入站生成了计量规则，outboundTag = %q", tag)
		}
	}
}

func TestInjectMeterIsByteStable(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32005)
	for _, d := range []string{"zeta.com", "alpha.com", "mid.com"} {
		putPoolRow(t, in.Id, d, 0)
	}
	first := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(first); err != nil {
		t.Fatalf("首次 Inject: %v", err)
	}
	second := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(second); err != nil {
		t.Fatalf("再次 Inject: %v", err)
	}
	// Config.Equals 按字节比较；顺序一抖动就恒判不等，那个 10 秒的 cron
	// 会不停重启 xray。
	if string(first.OutboundConfigs) != string(second.OutboundConfigs) {
		t.Errorf("出站不逐字节确定：\n%s\n%s", first.OutboundConfigs, second.OutboundConfigs)
	}
	if string(first.RouterConfig) != string(second.RouterConfig) {
		t.Errorf("路由不逐字节确定：\n%s\n%s", first.RouterConfig, second.RouterConfig)
	}
}

func TestInjectMeterCopiesRenamedDefaultOutbound(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32006)
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	cfg := newTemplateConfig(t)
	// 管理员给首个出站起过名字：tagDefaultOutbound 会原样保留它，
	// 计量出站必须是**那个**出站的副本，不能硬编码 freedom。
	cfg.OutboundConfigs = []byte(
		`[{"protocol":"freedom","tag":"我的直连","settings":{"domainStrategy":"UseIP"}},` +
			`{"protocol":"blackhole","settings":{},"tag":"blocked"}]`)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	meter := obs[len(obs)-1]
	if meter["tag"] != model.MeterTag(in.Id, "doubleclick.net") {
		t.Fatalf("最后一个出站 = %+v，期望是计量出站", meter)
	}
	settings, _ := meter["settings"].(map[string]any)
	if settings == nil || settings["domainStrategy"] != "UseIP" {
		t.Errorf("计量出站 settings = %v，期望复制了管理员那份（含 domainStrategy）", meter["settings"])
	}
}

func TestPoolChangeIsHotApplicable(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32007)
	putPoolRow(t, in.Id, "alpha.com", 0)

	before := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(before); err != nil {
		t.Fatalf("首次 Inject: %v", err)
	}
	putPoolRow(t, in.Id, "beta.com", 0)
	after := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(after); err != nil {
		t.Fatalf("换池后 Inject: %v", err)
	}

	// 换池必须能走热应用：出站增删（非首位）与整段路由替换都有控制面接口。
	// 走不了的话每小时换一次池就是每小时掐断一次所有人的连接。
	//
	// 最容易破坏这一点的是「计量出站不是深拷贝」——那会改到数组首位，
	// 而 diffOutbounds 要求默认出站逐字节不变，一变就必须整进程重启。
	diff, ok := xray.ComputeHotDiff(before, after)
	if !ok {
		t.Fatal("ComputeHotDiff 判定必须重启——换池应当能走热应用")
	}
	if len(diff.AddedOutbounds) != 1 {
		t.Errorf("新增出站 %d 个，期望 1", len(diff.AddedOutbounds))
	}
	if len(diff.RemovedOutboundTags) != 0 {
		t.Errorf("删除出站 %v，期望空", diff.RemovedOutboundTags)
	}
	if diff.RoutingConfig == nil {
		t.Error("路由配置未被标记为需要下发——新增的计量规则不会进入核心")
	}
	if len(diff.RemovedInboundTags) != 0 || len(diff.AddedInbounds) != 0 {
		t.Errorf("换池不该触碰入站：removed=%v added=%d",
			diff.RemovedInboundTags, len(diff.AddedInbounds))
	}
}

// jsonEq 比较一个来自 JSON 解码的值（[]any）与期望的字符串切片。
func jsonEq(t *testing.T, got any, want []string) bool {
	t.Helper()
	list, ok := got.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i := range want {
		if list[i] != want[i] {
			return false
		}
	}
	return true
}

func TestInjectSurvivesMeterPoolQueryFailure(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32008)
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	// 构造「用量库句柄还在、但查询本身失败」——这正是 Pool 里 db == nil 那条
	// 已经正确的 fail-open 路径覆盖不到的那一半。现实触发条件是 SQLITE_BUSY
	// 超过 go-sqlite3 默认 5s（用量库每 10 秒被三路写入、每小时还有一次可能
	// 扫过一年数据的清理 DELETE）、磁盘满、文件损坏。
	//
	// 这里 fail-close 的后果不是「日志多一行」：GetXrayConfig 一失败，
	// RestartXray 就在拿到配置那一步 return err，根本走不到 NewProcess；面板
	// 重启后 xray 本来就没在跑，CheckXrayRunningJob 每次触发都卡在同一处——
	// xray 永远起不来、全员断网。
	if err := database.GetTrafficDB().Exec("DROP TABLE meter_domains").Error; err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject 返回错误 %v——读池失败必须 fail-open 到「没有这个功能」，"+
			"绝不能让整份 xray 配置生成失败", err)
	}
	for _, ob := range decodeOutbounds(t, cfg) {
		if tag, _ := ob["tag"].(string); model.IsMeterTag(tag) {
			t.Errorf("读池失败却生成了计量出站 %q", tag)
		}
	}
	for _, r := range decodeRules(t, cfg) {
		if tag, _ := r["outboundTag"].(string); model.IsMeterTag(tag) {
			t.Errorf("读池失败却生成了计量规则，outboundTag = %q", tag)
		}
	}
}

func TestInjectSkipsPoolRowsThatAreNoLongerRegistrableDomains(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32009)
	// 写入路径（buildMeterCandidates）今天已经挡住了这种值，所以这一行只可能
	// 来自 publicsuffix 表升级后变成公共后缀本身的存量数据。生成端必须再挡
	// 一道：domain:com 会命中全部 .com，把该入站几乎全部流量吸进一个计量出站，
	// 榜单从此只有一行。Recompute 一小时内自愈，但那一小时里生成端照发。
	putPoolRow(t, in.Id, "com", 0)
	putPoolRow(t, in.Id, "example.com", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	badTag := model.MeterTag(in.Id, "com")
	goodTag := model.MeterTag(in.Id, "example.com")
	meterTags := make([]string, 0, 2)
	for _, ob := range decodeOutbounds(t, cfg) {
		if tag, _ := ob["tag"].(string); model.IsMeterTag(tag) {
			meterTags = append(meterTags, tag)
		}
	}
	if len(meterTags) != 1 || meterTags[0] != goodTag {
		t.Errorf("计量出站 = %v，期望只有 %q（%q 必须被跳过）", meterTags, goodTag, badTag)
	}
	// 出站与规则必须消费同一份过滤结果，否则会留下悬空 outboundTag——
	// xray 对此不报错，运行时静默回落默认出站。
	for _, r := range decodeRules(t, cfg) {
		if tag, _ := r["outboundTag"].(string); tag == badTag {
			t.Errorf("生成了引用 %q 的计量规则，而该出站并未写进配置", badTag)
		}
	}
}
