package service

import (
	"path/filepath"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

func setupDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func TestParseDomainsAcceptsNativeSyntax(t *testing.T) {
	raw := "domain:openai.com\nfull:chat.openai.com\ngeosite:openai\nregexp:.*\\.oaistatic\\.com"
	got, err := ParseDomains(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4: %v", len(got), got)
	}
	if got[0] != "domain:openai.com" {
		t.Errorf("got[0] = %q", got[0])
	}
}

func TestParseDomainsSkipsBlankLinesAndTrims(t *testing.T) {
	got, err := ParseDomains("  domain:a.com  \n\n\n  geosite:openai\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "domain:a.com" || got[1] != "geosite:openai" {
		t.Errorf("got = %v, want [domain:a.com geosite:openai]", got)
	}
}

func TestParseDomainsRejectsUnknownPrefix(t *testing.T) {
	if _, err := ParseDomains("wat:openai.com"); err == nil {
		t.Error("expected error for unknown prefix")
	}
}

func TestParseDomainsRejectsEmptyResult(t *testing.T) {
	if _, err := ParseDomains("   \n  \n"); err == nil {
		t.Error("expected error for empty domain list")
	}
}

func TestDomainGroupCRUD(t *testing.T) {
	setupDB(t)
	s := DomainGroupService{}

	encoded, err := EncodeDomains([]string{"domain:openai.com", "geosite:openai"})
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	g := &model.DomainGroup{Remark: "ChatGPT", Domains: encoded}
	if err := s.Add(g); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if g.Id == 0 {
		t.Fatal("Add did not assign an Id")
	}

	all, err := s.GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 || all[0].Remark != "ChatGPT" {
		t.Fatalf("GetAll = %v", all)
	}

	g.Remark = "OpenAI"
	if err := s.Update(g); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(g.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Remark != "OpenAI" {
		t.Errorf("Remark = %q, want OpenAI", got.Remark)
	}

	if err := s.Del(g.Id); err != nil {
		t.Fatalf("Del: %v", err)
	}
	all, _ = s.GetAll()
	if len(all) != 0 {
		t.Errorf("after Del, GetAll = %v, want empty", all)
	}
}

func TestMergeDomainsKeepsManualFirstAndDeduplicates(t *testing.T) {
	manual := []string{"domain:my-nas.local", "domain:qq.com"}
	subscribed := []string{"domain:qq.com", "domain:163.com"}
	got := MergeDomains(manual, subscribed)
	want := []string{"domain:my-nas.local", "domain:qq.com", "domain:163.com"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// 合并结果必须逐字节确定，否则 Config.Equals 恒为 false，
// 每 10 秒的重启 cron 会不停重启 xray。
func TestMergeDomainsIsDeterministic(t *testing.T) {
	manual := []string{"domain:b.com", "domain:a.com"}
	subscribed := []string{"domain:c.com", "domain:a.com", "domain:d.com"}
	first := MergeDomains(manual, subscribed)
	for i := 0; i < 50; i++ {
		again := MergeDomains(manual, subscribed)
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("run %d differs at %d: %q vs %q", i, j, first[j], again[j])
			}
		}
	}
}

func TestMergeDomainsHandlesEmptyInputs(t *testing.T) {
	if got := MergeDomains(nil, nil); len(got) != 0 {
		t.Errorf("both empty: got %v, want empty", got)
	}
	if got := MergeDomains([]string{"domain:a.com"}, nil); len(got) != 1 {
		t.Errorf("subscribed empty: got %v", got)
	}
	if got := MergeDomains(nil, []string{"domain:a.com"}); len(got) != 1 {
		t.Errorf("manual empty: got %v", got)
	}
}

func TestMergeDomainsDropsEmptyStrings(t *testing.T) {
	got := MergeDomains([]string{"", "domain:a.com"}, []string{"", ""})
	if len(got) != 1 || got[0] != "domain:a.com" {
		t.Errorf("got = %v, want [domain:a.com]", got)
	}
}

func TestParseDomainsAcceptsAllXrayPrefixes(t *testing.T) {
	raw := "domain:openai.com\nfull:chat.openai.com\nkeyword:openai\n" +
		"regexp:.*\\.oaistatic\\.com\ndotless:localhost\n" +
		"geosite:openai\next:geoip.dat:cn\next-domain:x.dat:tag\next-site:y.dat:tag"
	got, err := ParseDomains(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 9 {
		t.Fatalf("len = %d, want 9: %v", len(got), got)
	}
	if got[2] != "keyword:openai" {
		t.Errorf("got[2] = %q, want keyword:openai", got[2])
	}
}

// 管理员从小火箭配置里整段粘贴过来的原文，逐行都必须能过。
// 最后两行是 DOMAIN-KEYWORD 的转写，改动前正是它们让整段粘贴失败。
func TestParseDomainsAcceptsPastedShadowrocketBlock(t *testing.T) {
	raw := `domain:openai.com
domain:chatgpt.com
domain:chatgpt.site
domain:chat.com
domain:ai.com
domain:sora.com
domain:oaistatic.com
domain:oaiusercontent.com
domain:oaistatsig.com
domain:openaicom.imgix.net
domain:openaimerge.com
domain:crixet.com
domain:openaiapi-site.azureedge.net
domain:client-api.arkoselabs.com
full:openai-api.arkoselabs.com
full:chat.openai.com.cdn.cloudflare.net
full:openaicom-api-bdcpf8c6d2e9atf6.z01.azurefd.net
full:openaicomproductionae4b.blob.core.windows.net
full:production-openaicom-storage.azureedge.net
openai
chatgpt-async-webps`
	got, err := ParseDomains(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 21 {
		t.Fatalf("len = %d, want 21", len(got))
	}
	if got[19] != "keyword:openai" {
		t.Errorf("got[19] = %q, want keyword:openai", got[19])
	}
	if got[20] != "keyword:chatgpt-async-webps" {
		t.Errorf("got[20] = %q, want keyword:chatgpt-async-webps", got[20])
	}
}

// 含点的裸串在 xray 的 routing 规则里是子串匹配，在 geosite 数据文件里却是
// 后缀匹配。放行等于让从 geosite 列表复制来的 openai.com 静默变成能命中
// notopenai.com.evil.net 的规则，而没有任何一层会报错。
func TestParseDomainsRejectsAmbiguousBareDomain(t *testing.T) {
	_, err := ParseDomains("openai.com")
	if err == nil {
		t.Fatal("expected error for bare dotted string")
	}
	for _, want := range []string{"domain:openai.com", "full:openai.com", "keyword:openai.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}

// xray 只把目标域名转小写、不归一化配置里的模式
// （app/router/condition.go:59），所以大写的模式是永不命中的哑规则。
func TestParseDomainsLowercasesMatchableValues(t *testing.T) {
	got, err := ParseDomains("domain:OpenAI.COM\nfull:Chat.OpenAI.com\nkeyword:OpenAI\nOpenAI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:openai.com", "full:chat.openai.com", "keyword:openai", "keyword:openai"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// regexp: 与 dotless: 会被编译成正则，转小写会把 \D 变成 \d 这种
// 意义完全相反的东西。geosite:/ext: 的 code 由 xray 自己 ToUpper。
func TestParseDomainsKeepsCaseSensitiveForms(t *testing.T) {
	raw := "regexp:^API\\D+\\.Example\\.COM$\ndotless:LocalHost\ngeosite:OpenAI"
	got, err := ParseDomains(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"regexp:^API\\D+\\.Example\\.COM$", "dotless:LocalHost", "geosite:OpenAI"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseDomainsRejectsEmptyPrefixValue(t *testing.T) {
	if _, err := ParseDomains("domain:"); err == nil {
		t.Error("expected error for prefix with no value")
	}
}

func TestParseDomainsRejectsKeywordWithSeparators(t *testing.T) {
	if _, err := ParseDomains("open ai"); err == nil {
		t.Error("expected error for keyword containing a space")
	}
}

// updateFieldsFor 的列名单是手工维护的。漏掉一个列会让该字段静默地无法
// 通过编辑接口更新，而 Get 与展示完全正常——极易漏测，所以单独钉一条。
func TestUpdateFieldsForIncludesCidrs(t *testing.T) {
	old := &model.DomainGroup{Id: 1, Remark: "a", Domains: "[]", Cidrs: "[]"}
	next := &model.DomainGroup{Id: 1, Remark: "a", Domains: "[]", Cidrs: `["1.2.3.0/24"]`}
	fields := updateFieldsFor(old, next)
	if fields["cidrs"] != `["1.2.3.0/24"]` {
		t.Errorf("cidrs = %v, want the new value", fields["cidrs"])
	}
}

// 订阅地址变了，旧地址拉来的 IP 段继续参与分流就是「用错误的数据生效」。
func TestUpdateFieldsForClearsSubscribedCidrsWhenUrlChanges(t *testing.T) {
	old := &model.DomainGroup{Id: 1, SubscribeUrl: "https://a.example/x"}
	next := &model.DomainGroup{Id: 1, SubscribeUrl: "https://b.example/y"}
	fields := updateFieldsFor(old, next)
	if fields["subscribed_cidrs"] != "" {
		t.Errorf("subscribed_cidrs = %v, want cleared", fields["subscribed_cidrs"])
	}
	if fields["subscribed_domains"] != "" {
		t.Errorf("subscribed_domains = %v, want cleared", fields["subscribed_domains"])
	}
}

// TestDomainGroupUpdatePersistsCidrs 走一遍真实的 Add -> Update -> Get，
// 而不是像上面两条那样只测 updateFieldsFor 这个纯函数。这条测试钉的是
// updateFieldsFor 的列名单一旦漏掉 "cidrs"：Update 不会报任何错（GORM
// 的 Updates 对没提到的列不闻不问），但 Get 出来的 Cidrs 仍是改之前的
// 旧值——保存「成功」，内容却静默没变，这正是 CLAUDE.md 里点名的那类
// 后果最严重的 bug。同时改了 Remark，确认真正改动到的列不受影响。
func TestDomainGroupUpdatePersistsCidrs(t *testing.T) {
	setupDB(t)
	s := DomainGroupService{}

	g := &model.DomainGroup{Remark: "before", Domains: "[]", Cidrs: `["1.2.3.0/24"]`}
	if err := s.Add(g); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := s.Update(&model.DomainGroup{
		Id: g.Id, Remark: "after", Domains: "[]", Cidrs: `["8.8.8.8"]`,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(g.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Remark != "after" {
		t.Errorf("Remark = %q, want %q", got.Remark, "after")
	}
	if got.Cidrs != `["8.8.8.8"]` {
		t.Errorf("Cidrs = %q, want %q — updateFieldsFor 若漏掉 cidrs 列，这里会拿到 Update 之前的旧值",
			got.Cidrs, `["8.8.8.8"]`)
	}
}
