package service

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/xtls/xray-core/app/router"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/geodata"

	"a-ui/database/model"
	"a-ui/util/geodat"
	"a-ui/xray"
)

func resetGeoGenerations(t *testing.T) {
	t.Helper()
	clear := func() {
		geoGenMu.Lock()
		geoGenByCfg = map[weak.Pointer[xray.Config]]geoGeneration{}
		geoGenOrder = nil
		geoApplied = geoGeneration{}
		geoLastFlagged = ""
		geoGenMu.Unlock()
		(&XrayService{}).IsNeedRestartAndSetFalse()
	}
	clear()
	t.Cleanup(clear)
}

// 同一组省份、库里的段变了：dat 代码跟着变。xray 按「文件:代码」缓存 IP 集合，代码不变就一直用旧集合。
func TestGeoDatCodeChangesWithContent(t *testing.T) {
	in := regionInbound(1, `["江苏省"]`)
	p1 := buildGeoPlanWith([]*model.Inbound{in}, fakeProvinceDB{"江苏省": {"192.0.2.0/24"}}, nil, nil)
	p2 := buildGeoPlanWith([]*model.Inbound{in}, fakeProvinceDB{"江苏省": {"192.0.2.0/24", "198.51.100.0/24"}}, nil, nil)
	again := buildGeoPlanWith([]*model.Inbound{in}, fakeProvinceDB{"江苏省": {"192.0.2.0/24"}}, nil, nil)
	c1, c2 := p1.TagByInbound[1], p2.TagByInbound[1]
	if c1 == "" || c1 == c2 {
		t.Fatalf("内容变了代码必须变: %q vs %q", c1, c2)
	}
	if again.TagByInbound[1] != c1 {
		t.Errorf("同样的内容必须得到同样的代码（生成逐字节确定）: %q vs %q", again.TagByInbound[1], c1)
	}
	tag := regionTag([]string{"江苏省"})
	if geoTagOfCode(c1) != tag || geoTagOfCode(c2) != tag {
		t.Errorf("代码要能取回省份集合的 tag（last-known-good 的键）: %q %q", geoTagOfCode(c1), geoTagOfCode(c2))
	}
	if geoTagOfCode(tag) != tag {
		t.Errorf("升级前的 dat 里只有 tag，没有内容后缀，原样返回")
	}
	if p1.Entries[0].Tag != c1 {
		t.Errorf("dat 条目要用代码而不是 tag: %+v", p1.Entries)
	}
}

// TestXrayIPSetCacheCannotServeStaleContent 在面板进程里用 xray 自己的匹配器复现审查里的 R1-C1：文件内容变了、
// 代码没变，新建的匹配器命中缓存、拿到旧集合；换成按内容寻址的代码就拿到新集合。核心里是同一份代码。
func TestXrayIPSetCacheCannotServeStaleContent(t *testing.T) {
	xray.EnsureAssetLocation()
	name := "a-ui-geo-cachetest.dat"
	path := filepath.Join("bin", name)
	t.Cleanup(func() { _ = os.Remove(path) })
	write := func(entries []geodat.Entry) {
		t.Helper()
		data, err := geodat.Encode(entries)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	matcher := func(code string) *router.IPMatcher {
		t.Helper()
		rules, err := geodata.ParseIPRules([]string{"ext:" + name + ":" + code})
		if err != nil {
			t.Fatal(err)
		}
		m, err := router.NewIPMatcher(rules, router.MatcherAsType_Source)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	hits := func(m *router.IPMatcher, ip string) bool {
		return m.Apply(routercmd.AsRoutingContext(&routercmd.RoutingContext{SourceIPs: [][]byte{net.ParseIP(ip).To4()}}))
	}
	oldSet, newSet := []string{"192.0.2.0/24"}, []string{"198.51.100.0/24"}
	tag := regionTag([]string{"缓存测试"})

	write([]geodat.Entry{{Tag: tag, CIDRs: oldSet}})
	m1 := matcher(tag)
	write([]geodat.Entry{{Tag: tag, CIDRs: newSet}})
	m2 := matcher(tag)
	if !hits(m2, "192.0.2.7") || hits(m2, "198.51.100.7") {
		t.Skip("这个 xray 版本的匹配器不再按「文件:代码」缓存，前提不成立")
	}
	_ = m1 // 旧匹配器还被引用着：热应用时旧规则就是这样，缓存不会被回收

	newCode := geoDatCode(tag, newSet)
	write([]geodat.Entry{{Tag: newCode, CIDRs: newSet}})
	m3 := matcher(newCode)
	if hits(m3, "192.0.2.7") || !hits(m3, "198.51.100.7") {
		t.Fatal("按内容寻址的代码应当拿到新集合")
	}
}

// 写 dat 时保留正在运行的配置与最近几次生成引用的代码：进程重启、校验读的都是这一个文件。
func TestRetainedGeoEntries(t *testing.T) {
	existing := []geodatEntry{{Tag: "OLD", CIDRs: []string{"192.0.2.0/24"}}, {Tag: "GONE", CIDRs: []string{"198.51.100.0/24"}},
		{Tag: "CUR", CIDRs: []string{"203.0.113.0/24"}}}
	current := []geodatEntry{{Tag: "CUR", CIDRs: []string{"203.0.113.0/24"}}}
	keep := retainedGeoEntries(existing, current, map[string]bool{"OLD": true, "CUR": true})
	if len(keep) != 1 || keep[0].Tag != "OLD" {
		t.Fatalf("应只保留还被引用、又不在本轮里的 OLD: %+v", keep)
	}
}

func TestGeoCodesInRouting(t *testing.T) {
	routing := `{"rules":[{"type":"field","source":["::/0"]},{"type":"field","source":["ext:a-ui-geo.dat:!ALLOWAAAAAAAACBBBBBBBB"]},
		{"type":"field","source":["ext:geoip.dat:cn"]},{"type":"field","domain":["x.com"]}]}`
	got := geoCodesInRouting([]byte(routing))
	if len(got) != 1 || !got["ALLOWAAAAAAAACBBBBBBBB"] {
		t.Fatalf("geoCodesInRouting = %v", got)
	}
	if len(geoCodesInRouting(nil)) != 0 || len(geoCodesInRouting([]byte("{bad"))) != 0 {
		t.Error("没有或解不开的路由段返回空集")
	}
}

func TestLastGoodSeedsFromContentAddressedDat(t *testing.T) {
	cleanupGeoDat(t)
	tag := regionTag([]string{"江苏省"})
	cur := []string{"192.0.2.0/24"}
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: geoDatCode(tag, cur), CIDRs: cur},
		{Tag: geoDatCode(tag, []string{"198.51.100.0/24"}), CIDRs: []string{"198.51.100.0/24"}}}); err != nil {
		t.Fatal(err)
	}
	geoLastGoodMu.Lock()
	geoLastGood = map[string][]string{}
	geoLastGoodSeeded = false
	geoLastGoodMu.Unlock()
	t.Cleanup(func() { resetGeoLastGood(t) })
	got := lastGoodSnapshot()[tag]
	if strings.Join(got, ",") != strings.Join(cur, ",") {
		t.Fatalf("同一个 tag 有两份时取文件里的第一份（写入时本轮的排在前面）: %v", got)
	}
}

func TestGeoDegradedReportsAppliedNotGenerated(t *testing.T) {
	resetGeoGenerations(t)
	cfg := &xray.Config{}
	rememberGeoGeneration(cfg, geoGeneration{Degraded: map[int]GeoDegradation{1: {Mode: geoDegradeClosed, Reason: "x"}}})
	if len(GeoDegradedInbounds()) != 0 {
		t.Fatal("还没下发的生成结果不能当成核心的状态")
	}
	commitGeoApplied(cfg)
	if GeoDegradedInbounds()[1].Mode != geoDegradeClosed {
		t.Fatalf("核心用上之后才如实显示: %v", GeoDegradedInbounds())
	}
}

func TestGeoGenerationDifferentFromAppliedRequestsRestart(t *testing.T) {
	resetGeoGenerations(t)
	xs := &XrayService{}
	closedGen := geoGeneration{Degraded: map[int]GeoDegradation{1: {Mode: geoDegradeClosed, Reason: "IP 库未加载"}}}
	a := &xray.Config{}
	rememberGeoGeneration(a, closedGen)
	commitGeoApplied(a)
	xs.IsNeedRestartAndSetFalse()

	// 原因消失（IP 库加载好了）：生成结果与核心不同，要置重启标志，否则核心一直锁着这个入站。
	recovered := geoGeneration{Codes: map[int]string{1: "ALLOWAAAAAAAACBBBBBBBB"}}
	rememberGeoGeneration(&xray.Config{}, recovered)
	if !xs.IsNeedRestartAndSetFalse() {
		t.Fatal("生成结果与核心不同，应置重启标志")
	}
	// 同一份结果再生成一次（页面轮询、校验都会生成）：不重复置标志，免得失败时每 10 秒重试一次。
	b := &xray.Config{}
	rememberGeoGeneration(b, recovered)
	if xs.IsNeedRestartAndSetFalse() {
		t.Error("同一份结果不该重复置标志")
	}
	// 核心用上之后，同样的结果与核心一致：不再置标志。
	commitGeoApplied(b)
	rememberGeoGeneration(&xray.Config{}, recovered)
	if xs.IsNeedRestartAndSetFalse() {
		t.Error("与核心一致时不该置标志")
	}
}

func TestGeoBlockMatcherBlocksClosedInbound(t *testing.T) {
	resetGeoGenerations(t)
	cfg := &xray.Config{}
	rememberGeoGeneration(cfg, geoGeneration{Degraded: map[int]GeoDegradation{5: {Mode: geoDegradeClosed, Reason: "x"}}})
	commitGeoApplied(cfg)
	g := newGeoBlockMatcherWith([]*model.Inbound{{Id: 5, Enable: true, Regions: `["江苏省"]`}}, nil, allowedSetFor)
	if !g.Blocks(5, "192.0.2.7") || !g.Blocks(5, "2001:db8::1") {
		t.Fatal("锁死的入站，所有来源都是被地区规则拦下的")
	}
	if g.Blocks(5, "not-an-ip") {
		t.Error("非法来源仍为否")
	}
}

// 面板重启后 last-known-good 从 dat 做种子：残缺（partial）的条目在 dat 里有自己的标记，不能被当成完整的旧允许集。
func TestPartialEntriesAreNotSeededAsLastGood(t *testing.T) {
	cleanupGeoDat(t)
	resetGeoLastGood(t)
	db := fakeListedProvinceDB{fakeProvinceDB: fakeProvinceDB{"江苏省": {"58.192.0.0/11"}}, list: []string{"江苏省"}}
	in := &model.Inbound{Id: 1, Enable: true, Regions: `["江苏省","火星省"]`}
	plan := buildGeoPlanWith([]*model.Inbound{in}, db, nil, nil)
	if _, err := writeGeoDat(geoDatPath, plan.Entries); err != nil {
		t.Fatal(err)
	}
	geoLastGoodMu.Lock()
	geoLastGood = map[string][]string{}
	geoLastGoodSeeded = false
	geoLastGoodMu.Unlock()
	if got, ok := lastGoodSnapshot()[regionTag([]string{"江苏省", "火星省"})]; ok {
		t.Fatalf("残缺的允许集被当成了 last-known-good: %v", got)
	}
	if plan2 := buildGeoPlanWith([]*model.Inbound{in}, db, nil, lastGoodSnapshot()); plan2.Degraded[1].Mode != geoDegradePartial {
		t.Errorf("重启后仍应是 partial: %+v", plan2.Degraded[1])
	}
	if geoTagOfCode(plan.TagByInbound[1]) != regionTag([]string{"江苏省", "火星省"}) {
		t.Errorf("残缺条目的代码也要能取回 tag: %q", plan.TagByInbound[1])
	}
}

// 等待提交的生成结果最多留 8 份，但不能因此把 8 份完整的 *xray.Config 一直攥在内存里：带大规则集的配置
// 一份就是几 MB，小内存的 VPS 上白占几十 MB。只按身份认它，不让它活下去。
func TestGeoGenerationCacheDoesNotPinConfigs(t *testing.T) {
	resetGeoGenerations(t)
	func() {
		cfg := &xray.Config{}
		rememberGeoGeneration(cfg, geoGeneration{})
	}()
	runtime.GC()
	runtime.GC()
	geoGenMu.Lock()
	defer geoGenMu.Unlock()
	if len(geoGenOrder) == 0 {
		t.Fatal("没有记下这次生成")
	}
	for _, p := range geoGenOrder {
		if p.Value() != nil {
			t.Error("缓存还攥着已经没人用的配置")
		}
	}
}
