package service

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/entity"
)

func TestParseSubscriptionSurgeFormat(t *testing.T) {
	raw := `# ChinaMax
DOMAIN-SUFFIX,qq.com
DOMAIN,exact.example.com
DOMAIN-KEYWORD,baidu
IP-CIDR,1.1.1.1/32,no-resolve
PROCESS-NAME,Telegram
`
	domains, skipped, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:qq.com", "full:exact.example.com", "baidu"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, domains[i], want[i])
		}
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
}

func TestParseSubscriptionPlainDomainList(t *testing.T) {
	raw := ".360.cn\n163.com\n\n# comment\n.qq.com\n"
	domains, skipped, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:360.cn", "domain:163.com", "domain:qq.com"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, domains[i], want[i])
		}
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
}

func TestParseSubscriptionClashYaml(t *testing.T) {
	raw := "payload:\n  - '+.qq.com'\n  - \"163.com\"\n  - '.baidu.com'\n"
	domains, _, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:qq.com", "domain:163.com", "domain:baidu.com"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, domains[i], want[i])
		}
	}
}

func TestParseSubscriptionHandlesCRLF(t *testing.T) {
	domains, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\r\nDOMAIN-SUFFIX,163.com\r\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 2 || domains[0] != "domain:qq.com" || domains[1] != "domain:163.com" {
		t.Errorf("got = %v", domains)
	}
}

// 一条域名都解析不出来必须报错，绝不能返回空数组。
// 空的域名组会让 buildRule 跳过整条规则，流量静默退回直连——
// 这正是订阅源改格式或 URL 失效返回 404 页面时会走到的路径。
func TestParseSubscriptionRejectsEmptyResult(t *testing.T) {
	cases := map[string]string{
		"全是 IP 规则":  "IP-CIDR,1.1.1.1/32\nIP-CIDR6,::1/128\n",
		"全是注释":      "# nothing here\n# really\n",
		"空文本":       "   \n\n  \n",
		"404 HTML": "<!DOCTYPE html>\n<html><body>404: Not Found</body></html>\n",
	}
	for name, raw := range cases {
		if _, _, err := ParseSubscription(raw); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestParseSubscriptionDeduplicates(t *testing.T) {
	domains, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\n.qq.com\nqq.com\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("got = %v, want [domain:qq.com]", domains)
	}
}

func TestParseSubscriptionIgnoresTrailingPolicyField(t *testing.T) {
	domains, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com,DIRECT\nDOMAIN,a.com,PROXY,no-resolve\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 2 || domains[0] != "domain:qq.com" || domains[1] != "full:a.com" {
		t.Errorf("got = %v", domains)
	}
}

func TestParseSubscriptionRejectsGarbageEntries(t *testing.T) {
	// 含空格、斜杠、协议头的都不是域名，必须跳过而不是原样放进配置
	domains, skipped, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\nhttps://evil.com/path\nhas space\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("got = %v, want [domain:qq.com]", domains)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
}

// 纯 IP 字面量满足域名格式的所有其他检查（含点、无非法字符），但作为
// xray 的 domain 条件永远匹配不到任何流量——订阅一份纯 IP 列表不该「成功」。
func TestParseSubscriptionRejectsPlainIPLiterals(t *testing.T) {
	domains, skipped, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\n1.1.1.1\n8.8.8.8\n2001:4860:4860::8888\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("got = %v, want [domain:qq.com]", domains)
	}
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
}

// 部分订阅源（尤其 Windows 工具导出的）在文件开头带 UTF-8 BOM，
// 不去掉的话会粘在第一行规则类型前面，导致该行匹配全部失配、被误计成跳过。
func TestParseSubscriptionTrimsLeadingBOM(t *testing.T) {
	domains, skipped, err := ParseSubscription("\uFEFFDOMAIN-SUFFIX,qq.com\nDOMAIN-SUFFIX,163.com\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:qq.com", "domain:163.com"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, domains[i], want[i])
		}
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0 (BOM 不应被算成一条被跳过的规则)", skipped)
	}
}

func TestParseSubscriptionLowercasesKeyword(t *testing.T) {
	domains, _, err := ParseSubscription("DOMAIN-KEYWORD,BaiDu\nDOMAIN-KEYWORD,baidu\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 大小写变体必须归一后去重：域名匹配大小写不敏感，
	// 未归一的关键词在 xray 里可能永不命中。
	if len(domains) != 1 || domains[0] != "baidu" {
		t.Errorf("got = %v, want [baidu]", domains)
	}
}

func TestValidateSubscribeURLAcceptsHttpAndHttps(t *testing.T) {
	for _, u := range []string{"http://example.com/a.list", "https://example.com/a.list"} {
		if err := ValidateSubscribeURL(u); err != nil {
			t.Errorf("%s: unexpected error %v", u, err)
		}
	}
}

func TestValidateSubscribeURLRejectsOtherSchemes(t *testing.T) {
	for _, u := range []string{"ftp://example.com/a", "file:///etc/passwd", "example.com/a", ""} {
		if err := ValidateSubscribeURL(u); err == nil {
			t.Errorf("%s: expected error, got nil", u)
		}
	}
}

func TestFetchSubscriptionReadsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\n"))
	}))
	defer srv.Close()

	body, err := fetchSubscription(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(body, "qq.com") {
		t.Errorf("body = %q", body)
	}
}

func TestFetchSubscriptionRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if _, err := fetchSubscription(srv.URL); err == nil {
		t.Error("expected error for 404, got nil")
	}
}

// 不设上限的话一个大文件就能把面板打爆，而 cron 没有 panic 恢复，
// OOM 会杀掉整个面板进程。
func TestFetchSubscriptionRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < 11; i++ {
			w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	if _, err := fetchSubscription(srv.URL); err == nil {
		t.Error("expected error for oversized body, got nil")
	}
}

func TestRefreshWritesSubscribedDomainsOnSuccess(t *testing.T) {
	setupDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\nDOMAIN-SUFFIX,163.com\nIP-CIDR,1.1.1.1/32\n"))
	}))
	defer srv.Close()

	group := &model.DomainGroup{Remark: "国内", Domains: "[]", SubscribeUrl: srv.URL}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	s := &DomainGroupService{}
	if err := s.Refresh(group.Id); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	got, err := s.Get(group.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	domains, err := DecodeSubscribedDomains(got.SubscribedDomains)
	if err != nil {
		t.Fatalf("DecodeSubscribedDomains: %v", err)
	}
	if len(domains) != 2 || domains[0] != "domain:qq.com" {
		t.Errorf("domains = %v", domains)
	}
	if got.LastUpdatedAt == 0 {
		t.Error("LastUpdatedAt should be set")
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
	if got.LastSkipped != 1 {
		t.Errorf("LastSkipped = %d, want 1", got.LastSkipped)
	}
}

// 失败时清空订阅域名会让合并结果为空、规则被 buildRule 跳过、
// 流量静默退回直连。这是本功能最危险的失败模式，必须锁死。
func TestRefreshKeepsOldDataOnFailure(t *testing.T) {
	setupDB(t)

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"404", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }},
		{"解析为空", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<!DOCTYPE html><html>404: Not Found</html>"))
		}},
		{"空响应", func(w http.ResponseWriter, r *http.Request) {}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			const oldData = `["domain:old.com"]`
			group := &model.DomainGroup{
				Remark: tc.name, Domains: "[]", SubscribeUrl: srv.URL,
				SubscribedDomains: oldData, LastUpdatedAt: 1234567890,
			}
			if err := database.GetDB().Save(group).Error; err != nil {
				t.Fatalf("save group: %v", err)
			}

			s := &DomainGroupService{}
			if err := s.Refresh(group.Id); err == nil {
				t.Fatal("expected error, got nil")
			}

			got, err := s.Get(group.Id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.SubscribedDomains != oldData {
				t.Errorf("SubscribedDomains = %q, want %q (旧数据必须保留)",
					got.SubscribedDomains, oldData)
			}
			if got.LastUpdatedAt != 1234567890 {
				t.Errorf("LastUpdatedAt = %d, 失败不应改动成功时间", got.LastUpdatedAt)
			}
			if got.LastError == "" {
				t.Error("LastError 必须写入，否则管理员看不到订阅已经坏了")
			}
		})
	}
}

func TestRefreshRejectsGroupWithoutUrl(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{Remark: "无订阅", Domains: `["domain:a.com"]`}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	if err := (&DomainGroupService{}).Refresh(group.Id); err == nil {
		t.Error("expected error for group without subscribe url")
	}
}

// 从「国内域名合集」改成「广告拦截列表」之后，旧域名继续按新规则的动作生效
// 是一次用错误的数据分流，比规则暂时不生效更危险。
func TestUpdateClearsSubscribedDataWhenUrlChanges(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark: "组", Domains: "[]", SubscribeUrl: "http://a.example.com/list",
		SubscribedDomains: `["domain:old.com"]`, LastUpdatedAt: 111, LastSkipped: 5,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	s := &DomainGroupService{}
	err := s.Update(&model.DomainGroup{
		Id: group.Id, Remark: "组", Domains: "[]",
		SubscribeUrl: "http://b.example.com/list",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := s.Get(group.Id)
	if got.SubscribedDomains != "" {
		t.Errorf("SubscribedDomains = %q, want empty", got.SubscribedDomains)
	}
	if got.LastUpdatedAt != 0 {
		t.Errorf("LastUpdatedAt = %d, want 0 (触发立即重新拉取)", got.LastUpdatedAt)
	}
	if got.LastSkipped != 0 {
		t.Errorf("LastSkipped = %d, want 0", got.LastSkipped)
	}
}

func TestUpdateKeepsSubscribedDataWhenUrlUnchanged(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark: "组", Domains: "[]", SubscribeUrl: "http://a.example.com/list",
		SubscribedDomains: `["domain:old.com"]`, LastUpdatedAt: 111,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	s := &DomainGroupService{}
	err := s.Update(&model.DomainGroup{
		Id: group.Id, Remark: "改了备注", Domains: `["domain:manual.com"]`,
		SubscribeUrl: "http://a.example.com/list",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := s.Get(group.Id)
	if got.SubscribedDomains != `["domain:old.com"]` {
		t.Errorf("SubscribedDomains = %q, 地址没变不该清空", got.SubscribedDomains)
	}
	if got.LastUpdatedAt != 111 {
		t.Errorf("LastUpdatedAt = %d, want 111", got.LastUpdatedAt)
	}
	if got.Remark != "改了备注" || got.Domains != `["domain:manual.com"]` {
		t.Errorf("备注与手工域名应当被更新: %+v", got)
	}
}

// updateFieldsFor 是 Update 要写的列的纯函数版本，直接断言它的返回值。
//
// 这条不变量——「订阅地址没变时不碰订阅列」——用行为测试测不出来了：Update
// 现在是单条原子的 Updates(map) 语句，不再有 Get 之后、写入之前的窗口供测试
// 从外部并发改库来验证「没有被覆盖」。把字段映射抽成纯函数后可以直接检查
// 它到底往 map 里放了什么 key，这是唯一还能锁住这条不变量的方式。
func TestUpdateFieldsForKeepsSubscriptionColumnsWhenUrlUnchanged(t *testing.T) {
	old := &model.DomainGroup{
		Id: 1, Remark: "组", Domains: "[]", SubscribeUrl: "http://a.example.com/list",
		SubscribedDomains: `["domain:old.com"]`, LastUpdatedAt: 111, LastSkipped: 3,
	}
	next := &model.DomainGroup{
		Id: 1, Remark: "改了备注", Domains: `["domain:manual.com"]`,
		SubscribeUrl: "http://a.example.com/list",
	}

	got := updateFieldsFor(old, next)

	want := map[string]any{
		"remark":  "改了备注",
		"domains": `["domain:manual.com"]`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("updateFieldsFor() = %#v, want %#v (地址没变时不该碰订阅列)", got, want)
	}
}

func TestUpdateFieldsForClearsSubscriptionColumnsWhenUrlChanges(t *testing.T) {
	old := &model.DomainGroup{
		Id: 1, Remark: "组", Domains: "[]", SubscribeUrl: "http://a.example.com/list",
		SubscribedDomains: `["domain:old.com"]`, LastUpdatedAt: 111, LastError: "boom", LastSkipped: 3,
	}
	next := &model.DomainGroup{
		Id: 1, Remark: "组", Domains: "[]", SubscribeUrl: "http://b.example.com/list",
	}

	got := updateFieldsFor(old, next)

	want := map[string]any{
		"remark":             "组",
		"domains":            "[]",
		"subscribe_url":      "http://b.example.com/list",
		"subscribed_domains": "",
		"last_updated_at":    0,
		"last_error":         "",
		"last_skipped":       0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("updateFieldsFor() = %#v, want %#v", got, want)
	}
}

func TestDecodeSubscribedDomainsToleratesEmpty(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		got, err := DecodeSubscribedDomains(raw)
		if err != nil {
			t.Errorf("%q: unexpected error %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("%q: got %v, want empty", raw, got)
		}
	}
}

func TestSubscriptionUpdateTimeDefault(t *testing.T) {
	setupDB(t)
	got, err := (&SettingService{}).GetSubscriptionUpdateTime()
	if err != nil {
		t.Fatalf("GetSubscriptionUpdateTime: %v", err)
	}
	if got != "04:00" {
		t.Errorf("got = %q, want 04:00", got)
	}
}

func TestAllSettingRejectsBadUpdateTime(t *testing.T) {
	base := func(v string) *entity.AllSetting {
		return &entity.AllSetting{
			WebPort: 54321, WebBasePath: "/", TimeLocation: "Asia/Shanghai",
			// CheckValid 会先 json.Unmarshal 这个字段，空字符串会让它在
			// 到达时间格式校验之前就报错，测出来的就不是我们要测的东西了。
			XrayTemplateConfig:     "{}",
			SubscriptionUpdateTime: v,
		}
	}
	for _, bad := range []string{"25:00", "4:00pm", "0400", "", "04:60", "abc"} {
		if err := base(bad).CheckValid(); err == nil {
			t.Errorf("%q: expected error, got nil", bad)
		}
	}
	for _, good := range []string{"04:00", "00:00", "23:59"} {
		if err := base(good).CheckValid(); err != nil {
			t.Errorf("%q: unexpected error %v", good, err)
		}
	}
}

func TestShouldUpdateNow(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	today := func(h, m int) time.Time {
		return time.Date(2026, 9, 2, h, m, 0, 0, loc)
	}
	ms := func(tm time.Time) int64 { return tm.UnixMilli() }

	cases := []struct {
		name          string
		now           time.Time
		lastUpdatedAt int64
		want          bool
	}{
		{"从未成功过，未到时间点也要更新", today(1, 0), 0, true},
		{"已过时间点且今天没更新过", today(5, 0), ms(today(4, 0).AddDate(0, 0, -1)), true},
		{"已过时间点但今天更新过", today(5, 0), ms(today(4, 30)), false},
		{"未到时间点", today(3, 0), ms(today(0, 0).AddDate(0, 0, -1)), false},
		{"恰好到点", today(4, 0), ms(today(0, 0).AddDate(0, 0, -1)), true},
		{"今天更晚时候手动更新过", today(23, 0), ms(today(10, 0)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldUpdateNow(tc.now, tc.lastUpdatedAt, 4, 0)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRefreshDueOnlyTouchesGroupsWithUrl(t *testing.T) {
	setupDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\n"))
	}))
	defer srv.Close()

	withURL := &model.DomainGroup{Remark: "订阅组", Domains: "[]", SubscribeUrl: srv.URL}
	plain := &model.DomainGroup{Remark: "手工组", Domains: `["domain:a.com"]`}
	for _, g := range []*model.DomainGroup{withURL, plain} {
		if err := database.GetDB().Save(g).Error; err != nil {
			t.Fatalf("save group: %v", err)
		}
	}

	s := &DomainGroupService{}
	count, err := s.RefreshDue()
	if err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	got, _ := s.Get(withURL.Id)
	if got.SubscribedDomains == "" {
		t.Error("订阅组应当被更新")
	}
	gotPlain, _ := s.Get(plain.Id)
	if gotPlain.SubscribedDomains != "" || gotPlain.LastUpdatedAt != 0 {
		t.Error("没有订阅地址的组不应被碰")
	}
}

// 一个组拉取失败不能拖垮其余组。
func TestRefreshDueContinuesAfterFailure(t *testing.T) {
	setupDB(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\n"))
	}))
	defer good.Close()

	g1 := &model.DomainGroup{Remark: "坏", Domains: "[]", SubscribeUrl: bad.URL}
	g2 := &model.DomainGroup{Remark: "好", Domains: "[]", SubscribeUrl: good.URL}
	for _, g := range []*model.DomainGroup{g1, g2} {
		if err := database.GetDB().Save(g).Error; err != nil {
			t.Fatalf("save group: %v", err)
		}
	}

	s := &DomainGroupService{}
	count, err := s.RefreshDue()
	if err != nil {
		t.Fatalf("RefreshDue 不应因单个组失败而整体报错: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (只有好的那个算成功)", count)
	}
	if got, _ := s.Get(g2.Id); got.SubscribedDomains == "" {
		t.Error("好的组应当被更新")
	}
	if got, _ := s.Get(g1.Id); got.LastError == "" {
		t.Error("坏的组应当记录失败原因")
	}
}

// 订阅地址在拉取过程中被改掉时，这次拉取的结果必须作废。
// 否则组的 URL 是 B、域名却是 A 的内容，界面还显示「刚刚更新」——
// spec §5.5 把这种「用错误的数据分流」列为比规则不生效更危险。
func TestRefreshDiscardsResultWhenUrlChangedDuringFetch(t *testing.T) {
	setupDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,from-old-url.com\n"))
	}))
	defer srv.Close()

	group := &model.DomainGroup{Remark: "组", Domains: "[]", SubscribeUrl: srv.URL}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	// 模拟：管理员在拉取过程中把订阅地址改成了别的
	stale := &model.DomainGroup{
		Id: group.Id, Remark: group.Remark, Domains: group.Domains,
		SubscribeUrl: srv.URL, // refreshLocked 拿到的是旧地址
	}
	if err := database.GetDB().Model(model.DomainGroup{}).Where("id = ?", group.Id).
		Update("subscribe_url", "http://changed.example.com/list").Error; err != nil {
		t.Fatalf("simulate url change: %v", err)
	}

	s := &DomainGroupService{}
	subscriptionMu.Lock()
	err := s.refreshLocked(stale)
	subscriptionMu.Unlock()
	if err == nil {
		t.Error("订阅地址已变，本次结果应当作废并报错")
	}

	got, _ := s.Get(group.Id)
	if got.SubscribedDomains != "" {
		t.Errorf("SubscribedDomains = %q, 旧地址拉来的内容不得写入", got.SubscribedDomains)
	}
	if got.LastUpdatedAt != 0 {
		t.Errorf("LastUpdatedAt = %d, 不得标记为已成功更新", got.LastUpdatedAt)
	}
}
