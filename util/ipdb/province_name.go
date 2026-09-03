package ipdb

import "strings"

// canonicalProvinces 是本项目统一使用的中国省级地区写法。
//
// 它就是 ip2region 那一路数据里出现的原样名称——界面上的下拉框、入站的
// Regions 字段存的都是这些字符串，所以它是唯一的权威写法。别的数据源
// （纯真库用的是「江苏」这种短名）必须先归一到这里，否则同一个省会因为
// 写法不同被当成两个地区，多源合并直接失效。
var canonicalProvinces = []string{
	"北京市", "天津市", "上海市", "重庆市",
	"河北省", "山西省", "辽宁省", "吉林省", "黑龙江省",
	"江苏省", "浙江省", "安徽省", "福建省", "江西省", "山东省",
	"河南省", "湖北省", "湖南省", "广东省", "海南省",
	"四川省", "贵州省", "云南省", "陕西省", "甘肃省", "青海省",
	"台湾省",
	"内蒙古", "广西", "西藏", "宁夏", "新疆",
	"香港特别行政区", "澳门特别行政区",
}

// provinceSuffixes 是归一时要剥掉的后缀，长的排在前面，避免「特别行政区」
// 被「区」抢先匹配。
var provinceSuffixes = []string{"特别行政区", "自治区", "省", "市"}

// extraAliases 是靠剥后缀推不出来的写法。三个自治区的官方全称中间夹着民族名，
// 去掉「自治区」剩下的是「广西壮族」而不是「广西」。
var extraAliases = map[string]string{
	"广西壮族自治区":  "广西",
	"新疆维吾尔自治区": "新疆",
	"宁夏回族自治区":  "宁夏",
	"内蒙古自治区":   "内蒙古",
	"西藏自治区":    "西藏",
	"香港":       "香港特别行政区",
	"澳门":       "澳门特别行政区",
}

var provinceByKey = func() map[string]string {
	m := make(map[string]string, len(canonicalProvinces)*2+len(extraAliases))
	for _, full := range canonicalProvinces {
		m[full] = full
		m[provinceKey(full)] = full
	}
	for alias, full := range extraAliases {
		m[alias] = full
	}
	return m
}()

func provinceKey(name string) string {
	for _, suffix := range provinceSuffixes {
		if trimmed := strings.TrimSuffix(name, suffix); trimmed != name && trimmed != "" {
			return trimmed
		}
	}
	return name
}

// CanonicalProvince 把任意写法的省级地区名归一成本项目的标准写法。
// 第二个返回值为 false 表示这个字符串根本不是省级地区（比如「腾讯云」）。
func CanonicalProvince(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if full, ok := provinceByKey[name]; ok {
		return full, true
	}
	if full, ok := provinceByKey[provinceKey(name)]; ok {
		return full, true
	}
	return "", false
}

// CanonicalProvinces 返回全部标准省级地区名，供测试与界面校验使用。
func CanonicalProvinces() []string {
	out := make([]string, len(canonicalProvinces))
	copy(out, canonicalProvinces)
	return out
}
