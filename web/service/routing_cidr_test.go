package service

import (
	"strings"
	"testing"
)

func TestParseCidrsAcceptsXraySyntax(t *testing.T) {
	raw := "1.2.3.0/24\n8.8.8.8\n2001:db8::/32\n::1\ngeoip:cn\ngeoip:!cn\n!geoip:cn\next:geoip.dat:cn\next-ip:x.dat:tag"
	got, _, err := ParseCidrs(raw)
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
	_, _, err := ParseCidrs("openai.com")
	if err == nil {
		t.Fatal("expected error for a domain in the CIDR field")
	}
	if !strings.Contains(err.Error(), "域名") {
		t.Errorf("error should point the admin at the domain field, got: %v", err)
	}
}

func TestParseCidrsRejectsOutOfRangePrefix(t *testing.T) {
	if _, _, err := ParseCidrs("1.2.3.0/33"); err == nil {
		t.Error("expected error for IPv4 prefix > 32")
	}
	if _, _, err := ParseCidrs("2001:db8::/129"); err == nil {
		t.Error("expected error for IPv6 prefix > 128")
	}
}

func TestParseCidrsRejectsEmptyPrefixValue(t *testing.T) {
	if _, _, err := ParseCidrs("geoip:"); err == nil {
		t.Error("expected error for geoip: with no code")
	}
	if _, _, err := ParseCidrs("!"); err == nil {
		t.Error("expected error for a lone negation")
	}
}

func TestParseCidrsSkipsBlankLinesAndTrims(t *testing.T) {
	got, _, err := ParseCidrs("  1.2.3.0/24  \n\n\n  geoip:cn\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "1.2.3.0/24" || got[1] != "geoip:cn" {
		t.Errorf("got = %v", got)
	}
}

func TestParseCidrsRejectsEmptyResult(t *testing.T) {
	if _, _, err := ParseCidrs("  \n \n"); err == nil {
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

// IP 段与域名共用同一条去重规则：按归一后的完整字符串比，保留首次出现。
func TestParseCidrsDropsDuplicates(t *testing.T) {
	got, removed, err := ParseCidrs("1.2.3.0/24\n8.8.8.8\n1.2.3.0/24\ngeoip:cn\ngeoip:cn")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"1.2.3.0/24", "8.8.8.8", "geoip:cn"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q（顺序必须保留首次出现）", i, got[i], want[i])
		}
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
}

// 取反符号是规则的一部分，geoip:cn / geoip:!cn / !geoip:cn 是三条不同的规则。
// 去重若先剥掉 ! 再比较，会把「排除中国 IP」和「命中中国 IP」当成同一条，
// 静默删掉其中两条——分流范围被改写，而没有任何一层会报错。
func TestParseCidrsKeepsNegationVariantsApart(t *testing.T) {
	got, removed, err := ParseCidrs("geoip:cn\ngeoip:!cn\n!geoip:cn")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got = %v, want 3 条（取反写法不同就不是重复）", got)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}
