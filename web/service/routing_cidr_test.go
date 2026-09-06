package service

import (
	"strings"
	"testing"
)

func TestParseCidrsAcceptsXraySyntax(t *testing.T) {
	raw := "1.2.3.0/24\n8.8.8.8\n2001:db8::/32\n::1\ngeoip:cn\ngeoip:!cn\n!geoip:cn\next:geoip.dat:cn\next-ip:x.dat:tag"
	got, err := ParseCidrs(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 9 {
		t.Fatalf("len = %d, want 9: %v", len(got), got)
	}
	if got[0] != "1.2.3.0/24" || got[4] != "geoip:cn" {
		t.Errorf("got = %v", got)
	}
}

func TestParseCidrsRejectsDomains(t *testing.T) {
	_, err := ParseCidrs("openai.com")
	if err == nil {
		t.Fatal("expected error for a domain in the CIDR field")
	}
	if !strings.Contains(err.Error(), "域名") {
		t.Errorf("error should point the admin at the domain field, got: %v", err)
	}
}

func TestParseCidrsRejectsOutOfRangePrefix(t *testing.T) {
	if _, err := ParseCidrs("1.2.3.0/33"); err == nil {
		t.Error("expected error for IPv4 prefix > 32")
	}
	if _, err := ParseCidrs("2001:db8::/129"); err == nil {
		t.Error("expected error for IPv6 prefix > 128")
	}
}

func TestParseCidrsRejectsEmptyPrefixValue(t *testing.T) {
	if _, err := ParseCidrs("geoip:"); err == nil {
		t.Error("expected error for geoip: with no code")
	}
	if _, err := ParseCidrs("!"); err == nil {
		t.Error("expected error for a lone negation")
	}
}

func TestParseCidrsSkipsBlankLinesAndTrims(t *testing.T) {
	got, err := ParseCidrs("  1.2.3.0/24  \n\n\n  geoip:cn\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "1.2.3.0/24" || got[1] != "geoip:cn" {
		t.Errorf("got = %v", got)
	}
}

func TestParseCidrsRejectsEmptyResult(t *testing.T) {
	if _, err := ParseCidrs("  \n \n"); err == nil {
		t.Error("expected error for empty list")
	}
}

// nil 必须编码成 []，不能是 null：库里存 null 时列表页与导出侧要多一处
// 分支，而 [] 与「没有 IP 段」语义完全一致。
func TestEncodeCidrsNormalizesNil(t *testing.T) {
	got, err := EncodeCidrs(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "[]" {
		t.Errorf("got = %q, want []", got)
	}
}

// 没订阅过的组这一列本来就是空字符串，交给 DecodeCidrs 会得到 json 语法
// 错误，进而被 buildRule 当成「数据损坏」丢弃整条规则。
func TestDecodeSubscribedCidrsToleratesEmpty(t *testing.T) {
	got, err := DecodeSubscribedCidrs("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty", got)
	}
}
