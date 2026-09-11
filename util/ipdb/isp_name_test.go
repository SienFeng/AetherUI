package ipdb

import "testing"

func TestCanonicalISPUnifiesChineseCarriers(t *testing.T) {
	// 两个源对同一家运营商的写法不同，必须收敛到同一个字符串。
	// 不收敛的话 Multi.Lookup 的分歧判定（按字符串相等）会把它们当成
	// 两个不同的结论，「存疑」标签会对几乎每个 IP 亮起。
	for _, c := range []struct{ raw, want string }{
		{"电信", "中国电信"},
		{"中国电信", "中国电信"},
		{"CHINANET", "中国电信"},
		{"联通", "中国联通"},
		{"中国联通", "中国联通"},
		{"网通", "中国联通"},
		{"移动", "中国移动"},
		{"铁通", "中国移动"},
		{"中移铁通", "中国移动"},
		{"广电网", "中国广电"},
		{"教育网", "教育网"},
		{"中国教育网", "教育网"},
	} {
		if got := CanonicalISP(c.raw, false); got != c.want {
			t.Errorf("CanonicalISP(%q, false) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestCanonicalISPStripsSubLabelsAndPlaceholders(t *testing.T) {
	// 纯真库把同一家运营商按接入点拆成上万种写法（"电信/新世纪网吧"），
	// 不截断会让归属地种类撞上 65536 的格式上限、整次更新失败。
	for _, c := range []struct{ raw, want string }{
		{"电信/新世纪网吧", "中国电信"},
		{"移动/全省通用", "中国移动"},
		{"联通/联通信息港", "中国联通"},
		{" CZ88.NET", ""}, // 纯真库的广告占位
		{"0", ""},         // ip2region 的未知占位
		{"  ", ""},
		{"", ""},
	} {
		if got := CanonicalISP(c.raw, false); got != c.want {
			t.Errorf("CanonicalISP(%q, false) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestCanonicalISPKeepsUnknownChineseISPAsIs(t *testing.T) {
	// 地方运营商本身就是有效信息，映射表不可能穷举，认不出的原样保留。
	for _, raw := range []string{"鹏博士宽带", "华数宽带", "长城宽带"} {
		if got := CanonicalISP(raw, false); got != raw {
			t.Errorf("CanonicalISP(%q, false) = %q, want 原样保留", raw, got)
		}
	}
}

func TestCanonicalISPForeignKeepsOnlyKnownIDC(t *testing.T) {
	// 境外只保留云厂商：全量保留会让纯真库段数涨到 3.3 倍。
	// 上游常带公司全称，所以必须能按关键词命中，不能只做精确匹配。
	for _, c := range []struct{ raw, want string }{
		{"Google LLC", "Google"},
		{"Amazon Technologies Inc.", "Amazon"},
		{"Cloudflare, Inc.", "Cloudflare"},
		{"Microsoft Corporation", "Microsoft"},
		{"Philippine Long Distance Telephone Company", ""},
		{"中华电信", ""},
		{"", ""},
	} {
		if got := CanonicalISP(c.raw, true); got != c.want {
			t.Errorf("CanonicalISP(%q, true) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestCanonicalISPKeepsIDCInsideChina(t *testing.T) {
	// 境内的云厂商同样要认出来：把账号挂在国内云主机上中转，
	// 与家宽来源的含义完全不同。
	for _, c := range []struct{ raw, want string }{
		{"腾讯云", "腾讯云"},
		{"腾讯", "腾讯云"},
		{"阿里", "阿里云"},
		{"阿里云", "阿里云"},
		{"华为云", "华为云"},
	} {
		if got := CanonicalISP(c.raw, false); got != c.want {
			t.Errorf("CanonicalISP(%q, false) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// IsKnownIDC 的名单必须与 CanonicalISP 实际会产出的名字一致。两份名单漂开
// 之后，CanonicalISP 认出来的厂商这里认不出，画像会把一台云服务器当成普通
// 接入网络，而没有任何一层会报错。
func TestIsKnownIDCMatchesCanonicalISPOutput(t *testing.T) {
	cases := []struct {
		raw     string
		foreign bool
		want    bool
	}{
		{"Amazon Technologies Inc.", true, true},
		{"Google LLC", true, true},
		{"阿里云计算有限公司", false, true},
		{"腾讯云", false, true},
		{"中国电信", false, false},
		{"鹏博士宽带", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		canonical := CanonicalISP(c.raw, c.foreign)
		if got := IsKnownIDC(canonical); got != c.want {
			t.Errorf("IsKnownIDC(CanonicalISP(%q,%v)=%q) = %v, want %v",
				c.raw, c.foreign, canonical, got, c.want)
		}
	}
}

// 入参必须是归一后的名字。传原始值进去认不出来，这条钉住这个前提，
// 避免将来有人把 IsKnownIDC 接到未归一的字段上。
func TestIsKnownIDCRequiresCanonicalInput(t *testing.T) {
	if IsKnownIDC("Amazon Technologies Inc.") {
		t.Error("未归一的全称不该命中——IsKnownIDC 只吃 CanonicalISP 的输出")
	}
}
