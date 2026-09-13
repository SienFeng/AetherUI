package ipdb

import "testing"

// 拉丁名映射表是从 ip2region 交叉学出来的，重新生成时很容易引入本项目不认识的
// 写法。下面两条不变量把那类错误挡在编译期之后的第一道测试上——一旦省名跑出
// canonicalProvinces 的范围，多源合并与地区限制会同时失效，而界面上只表现为
// 「这个源查不到」，看不出是表的问题。

func TestLatinProvinceValuesAreCanonical(t *testing.T) {
	valid := map[string]bool{}
	for _, p := range CanonicalProvinces() {
		valid[p] = true
	}
	for latin, zh := range provinceByLatin {
		if !valid[zh] {
			t.Errorf("provinceByLatin[%q] = %q，不是 canonicalProvinces 里的写法", latin, zh)
		}
	}
}

func TestLatinCityProvinceKeysAreCanonical(t *testing.T) {
	valid := map[string]bool{}
	for _, p := range CanonicalProvinces() {
		valid[p] = true
	}
	for province := range cityByProvinceLatin {
		if !valid[province] {
			t.Errorf("cityByProvinceLatin 的省份键 %q 不是 canonicalProvinces 里的写法", province)
		}
	}
}

// 汉语拼音丢掉了声调与字形，同名不同地是真实存在的。用单键表会让其中一个静默
// 变成另一个，而界面、规则表、生成的配置全都渲染得完全正常，只是城市是错的。
func TestCityFromLatinDisambiguatesByProvince(t *testing.T) {
	cases := []struct{ province, latin, want string }{
		{"福建省", "Fuzhou", "福州市"},
		{"江西省", "Fuzhou", "抚州市"},
		{"浙江省", "Taizhou", "台州市"},
		{"江苏省", "Taizhou", "泰州市"},
	}
	for _, c := range cases {
		got, ok := CityFromLatin(c.province, c.latin)
		if !ok || got != c.want {
			t.Errorf("CityFromLatin(%q, %q) = %q, %v；想要 %q", c.province, c.latin, got, ok, c.want)
		}
	}
}

// DB-IP 把区名写在括号里，同一个城市因此有 "Guangzhou"、"Guangzhou (Yuexiu Qu)"、
// "Guangzhou (Tianhe Qu)" 好几个写法；IP2Location 偶尔带 " City" 后缀。不剥的话
// 每个写法都要在表里单独占一行，漏掉哪一行就是那批段查不到城市。
func TestCityFromLatinStripsAdministrativeSuffix(t *testing.T) {
	cases := []struct{ province, latin, want string }{
		{"广东省", "Guangzhou (Yuexiu Qu)", "广州市"},
		{"广东省", "Guangzhou (Tianhe Qu)", "广州市"},
		{"吉林省", "Jilin City", "吉林市"},
	}
	for _, c := range cases {
		got, ok := CityFromLatin(c.province, c.latin)
		if !ok || got != c.want {
			t.Errorf("CityFromLatin(%q, %q) = %q, %v；想要 %q", c.province, c.latin, got, ok, c.want)
		}
	}
}

// 认不出的城市必须留空。DB-IP 的城市字段里混着大量街道与区级地名，它们既不是
// 地级市、也无法从字面推回上级市；原样存入会让 sameLocation 把「Jinrongjie」与
// 「北京市」判成分歧，「存疑」标记会对几乎每个 IP 亮起。
func TestCityFromLatinRejectsUnknown(t *testing.T) {
	for _, latin := range []string{"Jinrongjie", "Wanghailou", "Youyilu", "Xiabancheng"} {
		if got, ok := CityFromLatin("北京市", latin); ok {
			t.Errorf("CityFromLatin(\"北京市\", %q) = %q, true；街道级地名必须留空", latin, got)
		}
	}
	if _, ok := CityFromLatin("不存在省", "Nanjing"); ok {
		t.Error("未知省份下必须返回 false")
	}
}

// 两个源的省名体系不同，必须都认。少收一种，那个源的对应省份会整省查不到，
// 而界面上只表现为「这个源没有数据」。
func TestProvinceFromLatinAcceptsBothRomanizations(t *testing.T) {
	cases := []struct{ latin, want string }{
		{"Nei Mongol", "内蒙古"},     // IP2Location 的官方罗马化
		{"Inner Mongolia", "内蒙古"}, // DB-IP 的英文惯用名
		{"Xizang", "西藏"},
		{"Tibet", "西藏"},
		{"Guangxi Zhuangzu", "广西"},
		{"Guangxi", "广西"},
		{"Ningxia Huizu", "宁夏"},
		{"Xinjiang Uygur", "新疆"},
	}
	for _, c := range cases {
		got, ok := ProvinceFromLatin(c.latin)
		if !ok || got != c.want {
			t.Errorf("ProvinceFromLatin(%q) = %q, %v；想要 %q", c.latin, got, ok, c.want)
		}
	}
	if _, ok := ProvinceFromLatin("Nowhere"); ok {
		t.Error("认不出的省名必须返回 false，不能原样存入")
	}
}

// ip2region 把港澳台放在「中国 | 香港特别行政区」这一层，而两个新源用的是独立
// 国家码。不做这层折叠，港澳台那七万多个段会被当成境外段、省份整个丢掉，地区
// 限制里选「香港特别行政区」时它们一票都不贡献。
func TestCountryFromCodeFoldsGreaterChina(t *testing.T) {
	for _, code := range []string{"CN", "HK", "TW", "MO"} {
		got, ok := CountryFromCode(code)
		if !ok || got != ChinaCountry {
			t.Errorf("CountryFromCode(%q) = %q, %v；想要 %q", code, got, ok, ChinaCountry)
		}
	}
	// 港澳台的省级写法也要认得出来，否则折叠成中国之后省份是空的。
	for _, c := range []struct{ latin, want string }{
		{"Hong Kong", "香港特别行政区"},
		{"Macao", "澳门特别行政区"},
		{"Taiwan", "台湾省"},
	} {
		if got, ok := ProvinceFromLatin(c.latin); !ok || got != c.want {
			t.Errorf("ProvinceFromLatin(%q) = %q, %v；想要 %q", c.latin, got, ok, c.want)
		}
	}
}

// 境外国家名沿用上游的英文写法，与 ip2region 对境外段的口径相同。翻成中文反而
// 会和现有两个源全部判成分歧。
func TestCountryFromCodeKeepsEnglishForForeign(t *testing.T) {
	cases := []struct{ code, want string }{
		{"AU", "Australia"},
		{"JP", "Japan"},
		{"US", "United States of America"},
	}
	for _, c := range cases {
		got, ok := CountryFromCode(c.code)
		if !ok || got != c.want {
			t.Errorf("CountryFromCode(%q) = %q, %v；想要 %q", c.code, got, ok, c.want)
		}
	}
	if _, ok := CountryFromCode("ZZ"); ok {
		t.Error("未收录的国家码必须返回 false，让调用方整条丢弃")
	}
}
