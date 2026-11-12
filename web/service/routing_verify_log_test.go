package service

import (
	"reflect"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// newGroupMatcher 直接构造一个匹配器（不经缓存），给只关心判定本身的测试用；某一侧条目为空时
// 不构造那一侧（其匹配恒为否）。生产代码从 smallMatchers / largeMatchers 取。
func newGroupMatcher(domains, cidrs []string) (*groupMatcher, error) {
	r, err := parseGroupRules(domains, cidrs)
	if err != nil {
		return nil, err
	}
	return r.build()
}

func TestGroupMatcher(t *testing.T) {
	m, err := newGroupMatcher(
		[]string{"domain:openai.com", "full:chatgpt.com", "keyword:gpt", "regexp:^api\\."},
		[]string{"1.2.3.0/24", "2001:db8::/32"},
	)
	if err != nil {
		t.Fatalf("newGroupMatcher: %v", err)
	}
	for _, target := range []string{
		"api.openai.com:443", "chatgpt.com:443", "mygpt.io:443", "api.x.com:80",
		"1.2.3.9:443", "[2001:db8::1]:443",
	} {
		if !m.Match(target) {
			t.Errorf("%s 应命中", target)
		}
	}
	for _, target := range []string{"notopenai.com:443", "5.6.7.8:443"} {
		if m.Match(target) {
			t.Errorf("%s 不应命中", target)
		}
	}
}

// full: 只匹配整串；keyword:gpt 会子串命中 sub.chatgpt.com，所以单独在不含关键词的组里验证。
func TestGroupMatcherFullIsExact(t *testing.T) {
	m, err := newGroupMatcher([]string{"full:chatgpt.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Match("sub.chatgpt.com:443") {
		t.Error("full: 不应命中子域名")
	}
}

func TestGroupMatcherOneSideEmpty(t *testing.T) {
	onlyIP, err := newGroupMatcher(nil, []string{"1.2.3.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if !onlyIP.Match("1.2.3.4:80") || onlyIP.Match("example.com:80") {
		t.Error("只有 IP 段时，域名一律不命中、IP 按段判断")
	}
	onlyDomain, err := newGroupMatcher([]string{"full:a.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !onlyDomain.Match("a.com:1") || onlyDomain.Match("1.2.3.4:80") {
		t.Error("只有域名时，IP 一律不命中")
	}
}

func TestGroupByTargetRoute(t *testing.T) {
	setupAccessLogDB(t)
	in := mkInbound(t, 50011)
	other := mkInbound(t, 50012)
	now := time.Now()
	add := func(inboundId int, target, route string, at time.Time) {
		t.Helper()
		row := &model.AccessLog{
			Time: at.UnixMilli(), InboundId: inboundId, Target: target, Route: route, Accepted: true,
		}
		if err := database.GetAccessLogDB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		add(in.Id, "chatgpt.com:443", "a-ui-us", now.Add(-time.Duration(i)*time.Minute))
	}
	add(in.Id, "chatgpt.com:443", "a-ui-default", now)
	add(in.Id, "chatgpt.com:443", "a-ui-us", now.Add(-25*time.Hour))
	add(other.Id, "chatgpt.com:443", "a-ui-us", now)

	s := AccessLogService{}
	since := now.Add(-24 * time.Hour).UnixMilli()
	rows, truncated, err := s.GroupByTargetRoute(in.Id, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(rows) != 2 || rows[0].Count != 3 || rows[1].Count != 1 {
		t.Fatalf("rows=%+v truncated=%v，期望 2 行计数 3、1", rows, truncated)
	}
	if rows[0].Route != "a-ui-us" || rows[1].LastAt != now.UnixMilli() {
		t.Errorf("分组内容不对: %+v", rows)
	}
	rows, truncated, err = s.GroupByTargetRoute(in.Id, since, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(rows) != 1 {
		t.Errorf("limit=1 应截断为 1 行，得到 %d 行 truncated=%v", len(rows), truncated)
	}
}

func TestBuildLogEvidence(t *testing.T) {
	m, err := newGroupMatcher([]string{"full:chatgpt.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const x = int64(1700000000000)
	rows := []TargetRouteCount{
		{Target: "chatgpt.com:443", Route: "a-ui-us", Accepted: true, Count: 3},
		{Target: "chatgpt.com:443", Route: "a-ui-us -> a-ui-default", Accepted: true, Count: 1},
		{Target: "chatgpt.com:443", Route: "a-ui-default", Accepted: true, Count: 2, LastAt: x},
		{Target: "example.com:443", Route: "a-ui-default", Accepted: true, Count: 9},
		{Target: "chatgpt.com:443", Route: "", Accepted: false, Count: 1},
	}
	exp := ExpectedOutbound{Action: model.ActionProxy, Tag: "a-ui-us"}
	got := buildLogEvidence(rows, m, exp, "direct", func(tag string) string { return "" }, nil)
	if got.Matched != 4 || got.Rejected != 1 {
		t.Fatalf("Matched=%d Rejected=%d，期望 4/1", got.Matched, got.Rejected)
	}
	want := []LogOther{{Route: "a-ui-default", Count: 2, LastAt: x, Examples: []string{"chatgpt.com:443"}}}
	if !reflect.DeepEqual(got.Others, want) {
		t.Errorf("Others = %+v，期望 %+v", got.Others, want)
	}
}

// 被入站地区限制拦下的封禁、核心没命中任何规则（" >> "）的连接都不是本规则走错：
// 各自单独计数，不进 Others，也不计入 Matched。
func TestBuildLogEvidenceSeparatesGeoBlockedAndNoRuleHit(t *testing.T) {
	m, err := newGroupMatcher([]string{"domain:xiaohongshu.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const x = int64(1700000000000)
	rows := []TargetRouteCount{
		{Target: "www.xiaohongshu.com:443", Route: "a-ui-sg", Accepted: true, Count: 5},
		// 地区限制外的来源：进了黑洞，但不是分流规则的事。
		{Target: "www.xiaohongshu.com:443", Route: model.BlockOutboundTag, Accepted: true, SourceIP: "203.0.113.81", Count: 7},
		// 允许集内的来源也进了黑洞：那是分流规则把它送进去的，仍算走错。
		{Target: "api.xiaohongshu.com:443", Route: model.BlockOutboundTag, Accepted: true, SourceIP: "198.51.100.77", Count: 2, LastAt: x},
		// 核心按嗅探到的另一个域名分流、没命中任何规则。
		{Target: "mall.xiaohongshu.com:443", Route: "a-ui-default", Accepted: true, NoRuleHit: true, Count: 3},
		// 升级前写入的行没有这个标记，按原口径算走错。
		{Target: "so.xiaohongshu.com:443", Route: "a-ui-default", Accepted: true, Count: 1, LastAt: x},
	}
	exp := ExpectedOutbound{Action: model.ActionProxy, Tag: "a-ui-sg"}
	geo := func(src string) bool { return src == "203.0.113.81" }
	got := buildLogEvidence(rows, m, exp, "a-ui-default", func(string) string { return "" }, geo)
	if got.Matched != 5 || got.GeoBlocked != 7 || got.NoRuleHit != 3 {
		t.Fatalf("Matched=%d GeoBlocked=%d NoRuleHit=%d，期望 5/7/3", got.Matched, got.GeoBlocked, got.NoRuleHit)
	}
	want := []LogOther{
		{Route: model.BlockOutboundTag, Count: 2, LastAt: x, Examples: []string{"api.xiaohongshu.com:443"}},
		{Route: "a-ui-default", Count: 1, LastAt: x, Examples: []string{"so.xiaohongshu.com:443"}},
	}
	if !reflect.DeepEqual(got.Others, want) {
		t.Errorf("Others = %+v，期望 %+v", got.Others, want)
	}
}

// 规则本身就是封禁时，进黑洞就是符合，不论来源是否在地区限制外。
func TestBuildLogEvidenceGeoBlockedOnBlockRuleIsMatch(t *testing.T) {
	m, err := newGroupMatcher([]string{"domain:speedtest.net"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := []TargetRouteCount{
		{Target: "www.speedtest.net:443", Route: model.BlockOutboundTag, Accepted: true, SourceIP: "203.0.113.81", Count: 4},
	}
	exp := ExpectedOutbound{Action: model.ActionBlock, Tag: model.BlockOutboundTag}
	got := buildLogEvidence(rows, m, exp, "a-ui-default", func(string) string { return "" }, func(string) bool { return true })
	if got.Matched != 4 || got.GeoBlocked != 0 || len(got.Others) != 0 {
		t.Errorf("Matched=%d GeoBlocked=%d Others=%+v，期望 4/0/空", got.Matched, got.GeoBlocked, got.Others)
	}
}
