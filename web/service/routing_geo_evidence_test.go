package service

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// writeTestGeoDat 在生产路径 bin/a-ui-geo.dat 写一份只含「北京市」允许集的 dat，
// 测试结束后删掉（连同为它新建的 bin 目录）。返回允许集对应的入站 regions 编码。
func writeTestGeoDat(t *testing.T, allow []string) string {
	t.Helper()
	dir := filepath.Dir(geoDatPath)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(dir) })
	}
	provinces := []string{"北京市"}
	// 与生产一样按内容寻址；核心还没用上过（没有已生效的状态）时匹配器按省份集合的 tag 在 dat 里找。
	resetGeoGenerations(t)
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: geoDatCode(regionTag(provinces), allow), CIDRs: allow}}); err != nil {
		t.Fatalf("writeGeoDat: %v", err)
	}
	cleanupGeoDat(t)
	regions, err := EncodeRegions(provinces)
	if err != nil {
		t.Fatal(err)
	}
	return regions
}

func TestGeoBlockMatcherMirrorsGeoRules(t *testing.T) {
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	limited := &model.Inbound{Id: 4, Enable: true, Regions: regions}
	open := &model.Inbound{Id: 5, Enable: true, Regions: "[]"}
	disabled := &model.Inbound{Id: 6, Enable: false, Regions: regions}
	g := newGeoBlockMatcher([]*model.Inbound{limited, open, disabled})

	cases := []struct {
		inbound int
		src     string
		want    bool
		why     string
	}{
		{4, "198.51.100.77", false, "允许集内的 IPv4 放行"},
		{4, "203.0.113.81", true, "允许集外的 IPv4 被拒"},
		{4, "2408:8000::1", true, "IPv6 来源一律被拒（a-ui-geo6 规则）"},
		{4, "not-an-ip", false, "认不出的来源不算地区拦截"},
		{5, "203.0.113.81", false, "没配地区限制的入站没有地区规则"},
		{6, "203.0.113.81", false, "停用入站不在核心配置里"},
	}
	for _, c := range cases {
		if got := g.Blocks(c.inbound, c.src); got != c.want {
			t.Errorf("%s：Blocks(%d, %s) = %v，期望 %v", c.why, c.inbound, c.src, got, c.want)
		}
	}
	if !g.isGeoBlocked(4, model.BlockOutboundTag, "203.0.113.81") || g.isGeoBlocked(4, "a-ui-sg", "203.0.113.81") {
		t.Error("只有确实进了黑洞的连接才可能是地区拦截")
	}
	var nilMatcher *geoBlockMatcher
	if nilMatcher.Blocks(4, "203.0.113.81") {
		t.Error("nil 匹配器一律为否")
	}
}

// dat 缺失（还没生成过）时不能报地区拦截：退回原口径，宁可多报也不藏真正走错的连接。
func TestGeoBlockMatcherWithoutDatFallsBack(t *testing.T) {
	regions, err := EncodeRegions([]string{"不存在的省份"})
	if err != nil {
		t.Fatal(err)
	}
	g := newGeoBlockMatcher([]*model.Inbound{{Id: 4, Enable: true, Regions: regions}})
	if g.Blocks(4, "203.0.113.81") || g.Blocks(4, "2408:8000::1") {
		t.Error("匹配器建不出来时不应判为地区拦截")
	}
}

// 列表上的异常摘要与流量走向图同口径：地区限制外来源的封禁、核心没命中任何规则的连接都不计。
func TestAnomalyScanSkipsGeoBlockedAndNoRuleHit(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	if err := database.GetDB().Model(f.a).Update("regions", regions).Error; err != nil {
		t.Fatal(err)
	}
	at := anomalyNow.Add(-30 * time.Minute)
	base := model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "api.openai.com:443", Accepted: true}

	geo := base
	geo.Route, geo.SourceIP = model.BlockOutboundTag, "203.0.113.81"
	insertLogs(t, 4, geo)
	noHit := base
	noHit.Route, noHit.SourceIP, noHit.NoRuleHit = "a-ui-default", "198.51.100.77", true
	insertLogs(t, 2, noHit)
	// 允许集内来源进了黑洞、命中规则后走了直连：这两类仍是本规则走错。
	inside := base
	inside.Route, inside.SourceIP = model.BlockOutboundTag, "198.51.100.77"
	insertLogs(t, 1, inside)
	wrong := base
	wrong.Route, wrong.SourceIP = "a-ui-default", "198.51.100.77"
	insertLogs(t, 3, wrong)

	mustScan(t, f.svc, anomalyNow, false)
	got := f.svc.Anomaly(f.rule.Id, anomalyNow)
	ua := got.ByUser[f.a.Id]
	if got.Count != 4 || ua == nil || ua.Routes[model.BlockOutboundTag] != 1 || ua.Routes["a-ui-default"] != 3 {
		t.Fatalf("Anomaly = %+v ByUser[A] = %+v，期望共 4 次：封禁 1、默认出站 3", got, ua)
	}

	// 流量走向图（Verify）对同一批行给出同样的划分。
	res, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime()).Verify(f.rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, f.a.Id)
	if u.Log == nil || u.Log.GeoBlocked != 4 || u.Log.NoRuleHit != 2 {
		t.Fatalf("Verify 日志证据 = %+v，期望 GeoBlocked=4 NoRuleHit=2", u.Log)
	}
	var others int64
	for _, o := range u.Log.Others {
		others += o.Count
	}
	if others != 4 || u.Verdict != verdictMismatch {
		t.Errorf("Others 共 %d 次、Verdict=%s，期望 4 次且仍为 mismatch（允许集内的走错照报）", others, u.Verdict)
	}
}

// 线上的样子：某用户换到地区限制外的 IP 后全部连接被拦，这条规则对他不应变红。
func TestVerifyOnlyGeoBlockedIsNotMismatch(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	if err := database.GetDB().Model(f.a).Update("regions", regions).Error; err != nil {
		t.Fatal(err)
	}
	at := anomalyNow.Add(-30 * time.Minute)
	insertLogs(t, 5, model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "api.openai.com:443",
		Route: model.BlockOutboundTag, SourceIP: "203.0.113.81", Accepted: true})
	insertLogs(t, 2, model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "api.openai.com:443",
		Route: f.node.Tag, SourceIP: "198.51.100.77", Accepted: true})

	mustScan(t, f.svc, anomalyNow, false)
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow); got.Count != 0 {
		t.Errorf("异常摘要不应计入地区拦截: %+v", got)
	}
	res, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime()).Verify(f.rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, f.a.Id)
	if u.Verdict != verdictVerified || u.Log == nil || u.Log.GeoBlocked != 5 || u.Log.Matched != 2 || len(u.Log.Others) != 0 {
		t.Errorf("Verdict=%s Log=%+v，期望 verified、GeoBlocked=5、Matched=2、无 Others", u.Verdict, u.Log)
	}
}
