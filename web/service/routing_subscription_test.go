package service

import "testing"

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
