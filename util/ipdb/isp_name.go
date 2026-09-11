package ipdb

import "strings"

// 本文件把两个数据源的运营商写法归一成一套统一名称，与 province_name.go
// 对省份做的事同构。
//
// 归一必须发生在落库前，不能挪到展示层，有两个各自独立的理由：
//
//  1. 纯真库的 ISP 字段有 115,619 种取值（"电信/新世纪网吧" 这类按接入点
//     拆出来的细分），原样入库会让归属地种类远超格式的 65536 上限，
//     BuildRecords 直接报错，整次更新失败并保留旧库——功能永远上不了线。
//  2. Multi.Lookup 的分歧判定是按字符串相等做的。ip2region 写「中国电信」、
//     纯真库写「电信」，不归一的话「存疑」标签会对几乎每个 IP 亮起，
//     把这个本来有用的提示废掉。

// ispAliases 是中国运营商的写法映射。key 一律小写，查表前先 ToLower。
var ispAliases = map[string]string{
	"电信": "中国电信", "中国电信": "中国电信", "chinanet": "中国电信",
	"china telecom": "中国电信", "中國電信": "中国电信",

	"联通": "中国联通", "中国联通": "中国联通", "网通": "中国联通",
	"中国网通": "中国联通", "china unicom": "中国联通", "unicom": "中国联通",

	"移动": "中国移动", "中国移动": "中国移动", "铁通": "中国移动",
	"中移铁通": "中国移动", "中国铁通": "中国移动", "china mobile": "中国移动",

	"广电网": "中国广电", "中国广电": "中国广电", "广电": "中国广电",

	"教育网": "教育网", "中国教育网": "教育网", "cernet": "教育网",
	"中国科技网": "科技网", "cstnet": "科技网",
}

// idcKeywords 是知名 IDC / 云厂商的关键词。上游常给公司全称
// （"Google LLC"、"Amazon Technologies Inc."），精确匹配命中不了，
// 必须按子串判定。
//
// 用切片而不是 map：一个字符串可能命中多个关键词，遍历顺序决定结果，
// 而 map 的遍历顺序是随机的——那会让同一份源数据每次生成出不同的库，
// 违反「生成逐字节确定」。
var idcKeywords = []struct{ keyword, name string }{
	{"cloudflare", "Cloudflare"},
	{"amazon", "Amazon"}, {"aws", "Amazon"}, {"亚马逊", "Amazon"},
	{"google", "Google"}, {"谷歌", "Google"},
	{"microsoft", "Microsoft"}, {"azure", "Microsoft"}, {"微软", "Microsoft"},
	{"akamai", "Akamai"}, {"阿卡迈", "Akamai"},
	{"fastly", "Fastly"},
	{"oracle", "Oracle"}, {"甲骨文", "Oracle"},
	{"digitalocean", "DigitalOcean"},
	{"linode", "Linode"},
	{"vultr", "Vultr"}, {"choopa", "Vultr"},
	{"hetzner", "Hetzner"},
	{"contabo", "Contabo"},
	{"leaseweb", "LeaseWeb"},
	{"scaleway", "Scaleway"},
	{"ovh", "OVH"},
	{"腾讯", "腾讯云"}, {"tencent", "腾讯云"},
	{"阿里", "阿里云"}, {"alibaba", "阿里云"}, {"aliyun", "阿里云"},
	{"华为", "华为云"}, {"huawei", "华为云"},
	{"百度", "百度云"}, {"baidu", "百度云"},
	{"ucloud", "UCloud"},
	{"金山", "金山云"}, {"kingsoft", "金山云"},
}

// cleanISP 做落库前的清洗：去空白、去掉两个源各自的「未知」占位，
// 并截掉纯真库那种 "电信/新世纪网吧" 的接入点后缀。
func cleanISP(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" {
		return ""
	}
	// CZ88.NET 是纯真库的广告占位，表示「这一段没有运营商信息」。
	if strings.Contains(strings.ToUpper(s), "CZ88") {
		return ""
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

func matchIDC(lower string) string {
	for _, e := range idcKeywords {
		if strings.Contains(lower, e.keyword) {
			return e.name
		}
	}
	return ""
}

// CanonicalISP 把任意写法的运营商名归一成本项目的统一名称。
//
// foreign 为 true（这条记录的国家不是中国）时只保留知名 IDC / 云厂商，
// 其余一律返回空串：境外段的 ISP 字段极其细碎，全量保留会让相邻段的合并
// 几乎完全失效，纯真库的段数从 261,670 涨到 858,363，文件与常驻内存跟着涨
// 3.3 倍。而境外来源 IP 真正有价值的信息就是「它是不是机房」。
func CanonicalISP(raw string, foreign bool) string {
	s := cleanISP(raw)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if idc := matchIDC(lower); idc != "" {
		return idc
	}
	if foreign {
		return ""
	}
	if isp, ok := ispAliases[lower]; ok {
		return isp
	}
	// 认不出的中国运营商原样保留：「鹏博士宽带」这类地方运营商本身就是
	// 有效信息，映射表不可能穷举。
	return s
}

// ChinaCountry 是本包对「中国」这个国家名的写法。
//
// 导出出来是因为消费侧要按「是不是中国段」分支：只有中国段带省份
// （normalize 对境外丢掉 Region/City），而各处自己写一个 "中国" 字面量
// 迟早与这里漂开，漂开之后的表现是境内段被当成境外段处理——省份信息
// 凭空消失，而没有任何一层会报错。
const ChinaCountry = chinaCountry

// idcNames 是 idcKeywords 里全部归一名的集合，供 IsKnownIDC 判定。
//
// 从 idcKeywords 生成而不是另写一份：两份名单迟早漂开，而漂开之后
// CanonicalISP 认出来的厂商这里认不出，画像会把一台云服务器当成普通
// 接入网络，风险信号静默失效。
var idcNames = func() map[string]bool {
	m := make(map[string]bool, len(idcKeywords))
	for _, e := range idcKeywords {
		m[e.name] = true
	}
	return m
}()

// IsKnownIDC 判断一个**已经归一过**的运营商名是不是知名 IDC / 云厂商。
//
// 入参必须是 CanonicalISP 的输出，不是上游原始值：原始值里
// "Amazon Technologies Inc." 这类全称在这里不会命中。
func IsKnownIDC(canonicalISP string) bool {
	return canonicalISP != "" && idcNames[canonicalISP]
}
