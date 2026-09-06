package service

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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
	domains, cidrs, skipped, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 关键词存显式的 keyword: 前缀，与手工录入路径（ParseDomains）的存储形态
	// 一致。两条路径形态不一致时 MergeDomains 去不掉重复。
	want := []string{"domain:qq.com", "full:exact.example.com", "keyword:baidu"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, domains[i], want[i])
		}
	}
	// IP-CIDR 现在被收进 IP 段而不是当成非域名规则跳过；只有 PROCESS-NAME
	// 仍不认识，跳过并计数——这是本任务引入的预期行为变化。
	if len(cidrs) != 1 || cidrs[0] != "1.1.1.1/32" {
		t.Errorf("cidrs = %v, want [1.1.1.1/32]", cidrs)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
}

func TestParseSubscriptionPlainDomainList(t *testing.T) {
	raw := ".360.cn\n163.com\n\n# comment\n.qq.com\n"
	domains, _, skipped, err := ParseSubscription(raw)
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
	domains, _, _, err := ParseSubscription(raw)
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
	domains, _, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\r\nDOMAIN-SUFFIX,163.com\r\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 2 || domains[0] != "domain:qq.com" || domains[1] != "domain:163.com" {
		t.Errorf("got = %v", domains)
	}
}

// 域名和 IP 段都解析不出来时必须报错，绝不能返回空数组。
// 空的分流组会让 buildRule 跳过整条规则，流量静默退回直连——
// 这正是订阅源改格式或 URL 失效返回 404 页面时会走到的路径。
//
// 「全是 IP 规则」不再属于这个集合：本任务之后 IP-CIDR/IP-CIDR6 会被解析
// 进 IP 段，属于合法的非空结果，覆盖场景见 TestParseSubscriptionCollectsIPRules。
func TestParseSubscriptionRejectsEmptyResult(t *testing.T) {
	cases := map[string]string{
		"全是注释":     "# nothing here\n# really\n",
		"空文本":      "   \n\n  \n",
		"404 HTML": "<!DOCTYPE html>\n<html><body>404: Not Found</body></html>\n",
	}
	for name, raw := range cases {
		if _, _, _, err := ParseSubscription(raw); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestParseSubscriptionDeduplicates(t *testing.T) {
	domains, _, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\n.qq.com\nqq.com\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("got = %v, want [domain:qq.com]", domains)
	}
}

func TestParseSubscriptionIgnoresTrailingPolicyField(t *testing.T) {
	domains, _, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com,DIRECT\nDOMAIN,a.com,PROXY,no-resolve\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 2 || domains[0] != "domain:qq.com" || domains[1] != "full:a.com" {
		t.Errorf("got = %v", domains)
	}
}

func TestParseSubscriptionRejectsGarbageEntries(t *testing.T) {
	// 含空格、斜杠、协议头的都不是域名，必须跳过而不是原样放进配置
	domains, _, skipped, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\nhttps://evil.com/path\nhas space\n")
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

// 纯 IP 字面量满足域名格式的所有其他检查（含点、无非法字符），isValidDomain
// 仍然拒绝把它们当域名——但本任务之后它们不再被当成垃圾丢弃计入 skipped，
// 而是落入 IP 段：无逗号的行先试 isValidCIDR，裸 IP 天然是合法的
// /32、/128 条件。
func TestParseSubscriptionPlainIPLiteralsBecomeCidrs(t *testing.T) {
	domains, cidrs, skipped, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\n1.1.1.1\n8.8.8.8\n2001:4860:4860::8888\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("domains = %v, want [domain:qq.com]", domains)
	}
	want := []string{"1.1.1.1", "8.8.8.8", "2001:4860:4860::8888"}
	if len(cidrs) != len(want) {
		t.Fatalf("cidrs = %v, want %v", cidrs, want)
	}
	for i := range want {
		if cidrs[i] != want[i] {
			t.Errorf("cidrs[%d] = %q, want %q", i, cidrs[i], want[i])
		}
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
}

// 部分订阅源（尤其 Windows 工具导出的）在文件开头带 UTF-8 BOM，
// 不去掉的话会粘在第一行规则类型前面，导致该行匹配全部失配、被误计成跳过。
func TestParseSubscriptionTrimsLeadingBOM(t *testing.T) {
	domains, _, skipped, err := ParseSubscription("\uFEFFDOMAIN-SUFFIX,qq.com\nDOMAIN-SUFFIX,163.com\n")
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
	domains, _, _, err := ParseSubscription("DOMAIN-KEYWORD,BaiDu\nDOMAIN-KEYWORD,baidu\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 大小写变体必须归一后去重：域名匹配大小写不敏感，
	// 未归一的关键词在 xray 里可能永不命中。
	if len(domains) != 1 || domains[0] != "keyword:baidu" {
		t.Errorf("got = %v, want [keyword:baidu]", domains)
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
		// IP-CIDR 现在会被解析进 SubscribedCidrs 而不是跳过；PROCESS-NAME 仍
		// 不认识，用它保留一条真正会被跳过的规则，不然 LastSkipped 测不出东西。
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\nDOMAIN-SUFFIX,163.com\nIP-CIDR,1.1.1.1/32\nPROCESS-NAME,Telegram\n"))
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
	// 两侧都写是本任务的核心行为：订阅解析出的 IP 段必须落到 SubscribedCidrs。
	cidrs, err := DecodeSubscribedCidrs(got.SubscribedCidrs)
	if err != nil {
		t.Fatalf("DecodeSubscribedCidrs: %v", err)
	}
	if len(cidrs) != 1 || cidrs[0] != "1.1.1.1/32" {
		t.Errorf("cidrs = %v, want [1.1.1.1/32]", cidrs)
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

// 设计 §5.3 的新不变量：**成功**的一次拉取必须把两侧都写掉，哪怕其中一侧
// 是空。它与「失败时绝不清空」（下一个测试）方向相反，两条必须同时立住——
// 上游真的不再列 IP 了还留着上一次的 IP，就是拿过期数据分流，比 IP 条件
// 消失更危险，而且不会有任何一层报错。
func TestRefreshClearsSubscribedCidrsWhenUpstreamDropsThem(t *testing.T) {
	setupDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\n"))
	}))
	defer srv.Close()

	// 先造出「上一次拉取带回了 IP 段」的状态。
	group := &model.DomainGroup{
		Remark: "国内", Domains: "[]", SubscribeUrl: srv.URL,
		SubscribedCidrs: `["1.1.1.1/32","8.8.8.0/24"]`,
		LastUpdatedAt:   1,
	}
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
	cidrs, err := DecodeSubscribedCidrs(got.SubscribedCidrs)
	if err != nil {
		t.Fatalf("DecodeSubscribedCidrs: %v", err)
	}
	if len(cidrs) != 0 {
		t.Errorf("上游这次一条 IP 都没给，旧的必须清掉，实际 %v", cidrs)
	}
	// 另一侧照常写入，确认清空不是「整次刷新没生效」造成的假象。
	domains, err := DecodeSubscribedDomains(got.SubscribedDomains)
	if err != nil {
		t.Fatalf("DecodeSubscribedDomains: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("domains = %v, want [domain:qq.com]", domains)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
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
		"cidrs":   "",
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
		"cidrs":              "",
		"subscribe_url":      "http://b.example.com/list",
		"subscribed_domains": "",
		"subscribed_cidrs":   "",
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
	// 用共用基线：CheckValid 逐条串行校验，任何一项不合法都会让本测试
	// 真正关心的那条走不到。
	base := func(v string) *entity.AllSetting {
		s := validBaseSetting()
		s.SubscriptionUpdateTime = v
		return s
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

// 手工录入与订阅拉取必须产出同一个字符串，否则 MergeDomains 去不掉重复。
func TestSubscriptionKeywordMatchesManualForm(t *testing.T) {
	fromSub, _, _, err := ParseSubscription("DOMAIN-KEYWORD,openai\n")
	if err != nil {
		t.Fatalf("ParseSubscription: %v", err)
	}
	fromManual, _, err := ParseDomains("openai")
	if err != nil {
		t.Fatalf("ParseDomains: %v", err)
	}
	if fromSub[0] != fromManual[0] {
		t.Errorf("subscription produced %q but manual produced %q", fromSub[0], fromManual[0])
	}
}

func TestParseSubscriptionCollectsIPRules(t *testing.T) {
	raw := `# comment
DOMAIN-SUFFIX,qq.com,DIRECT
IP-CIDR,1.1.1.0/24,PROXY,no-resolve
IP-CIDR6,2001:db8::/32,PROXY
GEOIP,CN,DIRECT
IP-ASN,20473,PROXY
SRC-IP-CIDR,192.168.1.0/24,DIRECT
PROCESS-NAME,Telegram,PROXY
`
	domains, cidrs, skipped, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("domains = %v", domains)
	}
	want := []string{"1.1.1.0/24", "2001:db8::/32", "geoip:cn"}
	if len(cidrs) != len(want) {
		t.Fatalf("cidrs = %v, want %v", cidrs, want)
	}
	for i := range want {
		if cidrs[i] != want[i] {
			t.Errorf("cidrs[%d] = %q, want %q", i, cidrs[i], want[i])
		}
	}
	// IP-ASN（xray 没有 ASN 匹配能力）、SRC-IP-CIDR（那是 source，按客户端 IP，
	// 塞进 ip 是语义错误）、PROCESS-NAME 三条必须跳过并计数。
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
}

// 纯 IP 列表（如中国 IP 段）从此是合法订阅源：改动前它会因「没解析出任何
// 域名」整份报错，而失败路径会保留上一次的数据，管理员看不出问题在哪。
func TestParseSubscriptionAcceptsPlainIPList(t *testing.T) {
	domains, cidrs, _, err := ParseSubscription("1.0.1.0/24\n1.0.2.0/23\n8.8.8.8\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 0 {
		t.Errorf("domains = %v, want empty", domains)
	}
	if len(cidrs) != 3 {
		t.Errorf("cidrs = %v, want 3 entries", cidrs)
	}
}

// 两侧都空才报错。报错是为了让调用方保留上一次成功的数据——上游改格式、
// URL 失效返回 404 页面、CDN 返回空响应都会走到这里。
func TestParseSubscriptionErrorsOnlyWhenBothEmpty(t *testing.T) {
	if _, _, _, err := ParseSubscription("IP-ASN,20473,PROXY\n"); err == nil {
		t.Error("expected error when nothing was parsed")
	}
}

func TestParseSubscriptionDedupesCidrs(t *testing.T) {
	_, cidrs, _, err := ParseSubscription("IP-CIDR,1.1.1.0/24,A\nIP-CIDR,1.1.1.0/24,B\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cidrs) != 1 {
		t.Errorf("cidrs = %v, want 1 entry", cidrs)
	}
}

func TestParseSubscribeURLs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"空值", "", nil},
		{"只有空白", "  \n\t\n ", nil},
		{"单个地址与本功能上线前一致", "https://a.com/x", []string{"https://a.com/x"}},
		{"多行去空白", " https://a.com/x \n\nhttps://b.com/y\n", []string{"https://a.com/x", "https://b.com/y"}},
		{"重复地址按首次出现去重", "https://a.com/x\nhttps://b.com/y\nhttps://a.com/x",
			[]string{"https://a.com/x", "https://b.com/y"}},
	}
	for _, tc := range cases {
		got := ParseSubscribeURLs(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("%s: 解析出 %v，期望 %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: 第 %d 个 = %q，期望 %q（顺序即合并顺序，不能重排）",
					tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestValidateSubscribeURLs(t *testing.T) {
	// 空值必须放行：它表示这个组不订阅，拒绝的话所有不订阅的域名组都保存不了。
	if err := ValidateSubscribeURLs(""); err != nil {
		t.Errorf("空值应放行: %v", err)
	}
	if err := ValidateSubscribeURLs("https://a.com/x\nhttps://b.com/y"); err != nil {
		t.Errorf("两个合法地址应放行: %v", err)
	}
	// 任何一行非法都要拒绝，不能只看第一行。
	if err := ValidateSubscribeURLs("https://a.com/x\nftp://b.com/y"); err == nil {
		t.Error("第二行是 ftp://，应被拒绝")
	}
	var many []string
	for i := 0; i <= maxSubscribeURLs; i++ {
		many = append(many, "https://a.com/"+strconv.Itoa(i))
	}
	if err := ValidateSubscribeURLs(strings.Join(many, "\n")); err == nil {
		t.Errorf("超过 %d 个地址应被拒绝", maxSubscribeURLs)
	}
}

// 多个地址的内容合并成一份，按地址顺序有序去重。
func TestRefreshMergesAllSubscribeURLs(t *testing.T) {
	setupDB(t)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\nIP-CIDR,1.1.1.0/24,no-resolve\n"))
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// qq.com 与 a 重复，必须去重且只保留首次出现
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\nDOMAIN-SUFFIX,taobao.com\n"))
	}))
	defer b.Close()

	group := &model.DomainGroup{
		Remark: "国内", Domains: "[]", SubscribeUrl: a.URL + "\n" + b.URL,
	}
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
	want := []string{"domain:qq.com", "domain:taobao.com"}
	if len(domains) != len(want) {
		t.Fatalf("domains = %v，期望 %v", domains, want)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q，期望 %q（按地址顺序合并，去重保留首次出现）",
				i, domains[i], want[i])
		}
	}
	cidrs, err := DecodeSubscribedCidrs(got.SubscribedCidrs)
	if err != nil {
		t.Fatalf("DecodeSubscribedCidrs: %v", err)
	}
	if len(cidrs) != 1 || cidrs[0] != "1.1.1.0/24" {
		t.Errorf("cidrs = %v，期望 [1.1.1.0/24]（只有第一个地址给了 IP 段）", cidrs)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
}

// 一个地址失败时，它上一次的内容原样保留并继续参与合并；其余地址照常更新到
// 最新。这是「每个地址相互独立」的核心语义，也是本功能全部的存在理由。
//
// 做不到这一点的实现只有两种退化形态，都更糟：整次不写（失败地址拖住所有人），
// 或者只写成功地址的内容（失败地址上一次的内容被清空，合并结果可能因此变空，
// buildRule 跳过整条规则，流量静默退回直连）。
func TestRefreshKeepsFailedURLContentAndUpdatesOthers(t *testing.T) {
	setupDB(t)
	aBody := "DOMAIN-SUFFIX,a1.com\n"
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(aBody))
	}))
	defer a.Close()
	bFails := false
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bFails {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte("DOMAIN-SUFFIX,b.com\n"))
	}))
	defer b.Close()

	group := &model.DomainGroup{Remark: "混合", Domains: "[]", SubscribeUrl: a.URL + "\n" + b.URL}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	s := &DomainGroupService{}

	// 第一轮：两个地址都成功，各自的内容都进了合并结果。
	if err := s.Refresh(group.Id); err != nil {
		t.Fatalf("首次 Refresh: %v", err)
	}
	if got := mustSubscribedDomains(t, s, group.Id); !reflect.DeepEqual(got, []string{"domain:a1.com", "domain:b.com"}) {
		t.Fatalf("首次合并结果 = %v，期望 [domain:a1.com domain:b.com]", got)
	}

	// 第二轮：a 换了内容，b 挂了。
	aBody = "DOMAIN-SUFFIX,a2.com\n"
	bFails = true
	err := s.Refresh(group.Id)
	if err == nil {
		t.Fatal("有地址失败时必须如实返回错误，否则管理员会以为这次刷新完全成功")
	}
	if !strings.Contains(err.Error(), b.URL) {
		t.Errorf("错误里应点名失败的地址 %s，实际 %q", b.URL, err.Error())
	}

	got := mustSubscribedDomains(t, s, group.Id)
	want := []string{"domain:a2.com", "domain:b.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("合并结果 = %v，期望 %v：a 应更新到最新，b 应保留上一次的内容", got, want)
	}

	// b 自己那一行：内容原样保留，LastUpdatedAt 不推进，只记 LastError。
	states, err := s.subscriptionStates(group.Id)
	if err != nil {
		t.Fatalf("subscriptionStates: %v", err)
	}
	bs := states[b.URL]
	if bs == nil {
		t.Fatal("b 的状态行不见了")
	}
	if bs.Domains != `["domain:b.com"]` {
		t.Errorf("b.Domains = %q，失败不该动它的内容", bs.Domains)
	}
	if bs.LastError == "" {
		t.Error("b.LastError 应记下这次的失败原因")
	}
	as := states[a.URL]
	if as == nil || as.LastError != "" {
		t.Errorf("a 这次成功了，它的 LastError 必须被清掉，实际 %+v", as)
	}
	if as.LastUpdatedAt <= bs.LastUpdatedAt {
		t.Errorf("a 的 LastUpdatedAt(%d) 应晚于 b 的(%d)：只有 a 这次拉成功了",
			as.LastUpdatedAt, bs.LastUpdatedAt)
	}
}

// 所有地址都失败且都没有历史内容时，绝不写回——那会把上一次的合并结果清空，
// buildRule 因「域名组为空」跳过整条规则，流量静默退回直连。
//
// 这是「每个地址独立」下唯一一处仍然整次不写的地方。
func TestRefreshWritesNothingWhenAllURLsFail(t *testing.T) {
	setupDB(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	group := &model.DomainGroup{
		Remark: "国内", Domains: "[]", SubscribeUrl: bad.URL,
		SubscribedDomains: `["domain:old.com"]`,
		SubscribedCidrs:   `["9.9.9.0/24"]`,
		LastUpdatedAt:     1234,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	s := &DomainGroupService{}
	if err := s.Refresh(group.Id); err == nil {
		t.Fatal("地址返回 500，Refresh 必须报错")
	}
	got, err := s.Get(group.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SubscribedDomains != `["domain:old.com"]` {
		t.Errorf("SubscribedDomains = %q，必须原样保留", got.SubscribedDomains)
	}
	if got.SubscribedCidrs != `["9.9.9.0/24"]` {
		t.Errorf("SubscribedCidrs = %q，必须原样保留", got.SubscribedCidrs)
	}
	if got.LastUpdatedAt != 1234 {
		t.Errorf("LastUpdatedAt = %d，失败不该推进它", got.LastUpdatedAt)
	}
}

// 删除域名组必须连带删掉它的订阅结果行。
//
// SQLite 会复用被删除的自增 id：残留的行会绑到下一个新建的组上，那个组会莫名
// 其妙带着别人的订阅内容参与分流，而引用不再悬空，生成期那道「跳过不存在的
// 引用」的防线拦不住它。
func TestDelDomainGroupDeletesItsSubscriptionRows(t *testing.T) {
	setupDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\n"))
	}))
	defer srv.Close()

	s := &DomainGroupService{}
	group := &model.DomainGroup{Remark: "国内", Domains: "[]", SubscribeUrl: srv.URL}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	if err := s.Refresh(group.Id); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	oldId := group.Id
	if err := s.Del(oldId); err != nil {
		t.Fatalf("Del: %v", err)
	}

	states, err := s.subscriptionStates(oldId)
	if err != nil {
		t.Fatalf("subscriptionStates: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("删组后还剩 %d 行订阅结果，SQLite 复用 id 时它们会绑到下一个新建的组上", len(states))
	}
}

func mustSubscribedDomains(t *testing.T, s *DomainGroupService, groupId int) []string {
	t.Helper()
	g, err := s.Get(groupId)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	list, err := DecodeSubscribedDomains(g.SubscribedDomains)
	if err != nil {
		t.Fatalf("DecodeSubscribedDomains: %v", err)
	}
	return list
}
