package service

import (
	"path/filepath"
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
