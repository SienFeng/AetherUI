package service

import (
	"net"
	"testing"

	"a-ui/util/ipdb"
)

func srcLoc(source, region string) ipdb.SourceLocation {
	return ipdb.SourceLocation{
		Source:   source,
		Location: ipdb.Location{Country: ipdb.ChinaCountry, Region: region},
	}
}

func TestEvidenceOf(t *testing.T) {
	cases := []struct {
		name                      string
		locs                      []ipdb.SourceLocation
		total                     int
		province                  string
		support, conflict, silent int
	}{
		{
			name: "四源全票",
			locs: []ipdb.SourceLocation{
				srcLoc("a", "江苏省"), srcLoc("b", "江苏省"),
				srcLoc("c", "江苏省"), srcLoc("d", "江苏省"),
			},
			total: 4, province: "江苏省", support: 4,
		},
		{
			name: "三比一",
			locs: []ipdb.SourceLocation{
				srcLoc("a", "江苏省"), srcLoc("b", "江苏省"),
				srcLoc("c", "江苏省"), srcLoc("d", "山东省"),
			},
			total: 4, province: "江苏省", support: 3, conflict: 1,
		},
		{
			// 这一条是本结构存在的**全部理由**：把 Silent 并进 Support 的话，
			// 它会显示成「4/4 一致」——而实际上只有两个源看过这个 IP。最不该
			// 被信任的一类判定会被包装成最可信的。
			name: "两源支持 + 两源没有数据",
			locs: []ipdb.SourceLocation{
				srcLoc("a", "江苏省"), srcLoc("b", "江苏省"),
			},
			total: 4, province: "江苏省", support: 2, conflict: 0, silent: 2,
		},
		{
			name: "收录了该 IP 但没有省份（境外段）也算 Silent",
			locs: []ipdb.SourceLocation{
				{Source: "a", Location: ipdb.Location{Country: "Australia"}},
				{Source: "b", Location: ipdb.Location{Country: "Australia"}},
			},
			total: 2, province: "", support: 0, conflict: 0, silent: 2,
		},
		{
			name:  "一个源都没有",
			locs:  nil,
			total: 4, province: "", silent: 4,
		},
		{
			name:  "单源覆盖",
			locs:  []ipdb.SourceLocation{srcLoc("a", "新疆")},
			total: 4, province: "新疆", support: 1, silent: 3,
		},
		{
			// 平票取字节序最小的，只为让同一份库对同一个 IP 永远给出同一个
			// 展示结果——这里不承担仲裁职责，取谁都不影响主判定。
			// 注意是**字节序**不是拼音序：UTF-8 下 "山"(E5B1B1) < "广"(E5B9BF)。
			name: "二比二平票取字节序",
			locs: []ipdb.SourceLocation{
				srcLoc("a", "广东省"), srcLoc("b", "广东省"),
				srcLoc("c", "山东省"), srcLoc("d", "山东省"),
			},
			total: 4, province: "山东省", support: 2, conflict: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := evidenceOf(c.locs, c.total)
			if got.Province != c.province || got.Support != c.support ||
				got.Conflict != c.conflict || got.Silent != c.silent || got.Total != c.total {
				t.Errorf("= %+v\n想要 Province=%q Support=%d Conflict=%d Silent=%d Total=%d",
					got, c.province, c.support, c.conflict, c.silent, c.total)
			}
			// 三个计数必须恰好覆盖所有源，一个不多一个不少。漏掉会让界面上的
			// 分母对不上，多算会把同一个源数两次。
			if sum := got.Support + got.Conflict + got.Silent; sum != c.total {
				t.Errorf("Support+Conflict+Silent = %d，应当等于 Total %d", sum, c.total)
			}
		})
	}
}

// 接线测试：证据必须真的从 locateWithIPDB 流出来，而不是停在 evidenceOf 里。
func TestLocateWithIPDBFillsEvidence(t *testing.T) {
	svc := ipdbServiceWithSources(t,
		buildLocTestDB(t, "江苏省", "南京市", "中国电信"),
		buildLocTestDB(t, "山东省", "济南市", "中国联通"))

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))
	if got.Evidence.Total != 2 {
		t.Errorf("Total = %d，想要 2", got.Evidence.Total)
	}
	if got.Evidence.Support != 1 || got.Evidence.Conflict != 1 {
		t.Errorf("= %+v；两个源各执一词时应当是 1 支持 1 冲突", got.Evidence)
	}
	// 主判定仍由源顺序决定，与票数无关——这正是本设计刻意保留的分工。
	if got.Location == "" {
		t.Error("主判定不应为空")
	}
}
