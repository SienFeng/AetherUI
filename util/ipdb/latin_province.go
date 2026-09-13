package ipdb

import "strings"

// provinceByLatin 把两个拉丁源的省级地区名映射到本项目的标准中文写法。
//
// 两个源的写法体系不同，必须都收进来：IP2Location 用官方罗马化
// （Nei Mongol / Xizang / Guangxi Zhuangzu），DB-IP 用英文惯用名
// （Inner Mongolia / Tibet / Guangxi）。少收一种，那个源的对应省份就会
// 整省查不到，而界面上只表现为「这个源没有数据」，看不出是映射缺失。
//
// 香港的十八区、台湾的各县市、澳门的各堂区也都在表里，一律折叠到省级：
// 这两个源把它们放在 region 一层，而本项目的省级写法只到「香港特别行政区」。
var provinceByLatin = map[string]string{
	"Anhui":                  "安徽省",
	"Central and Western":    "香港特别行政区",
	"Changhua":               "台湾省",
	"Chiayi":                 "台湾省",
	"Chongqing":              "重庆市",
	"Eastern":                "香港特别行政区",
	"Eastern District":       "香港特别行政区",
	"Fujian":                 "福建省",
	"Fukien":                 "台湾省",
	"Gansu":                  "甘肃省",
	"Guangdong":              "广东省",
	"Guangxi":                "广西",
	"Guangxi Zhuangzu":       "广西",
	"Guizhou":                "贵州省",
	"Hainan":                 "海南省",
	"Hebei":                  "河北省",
	"Heilongjiang":           "黑龙江省",
	"Henan":                  "河南省",
	"Hong Kong":              "香港特别行政区",
	"Hsinchu":                "台湾省",
	"Hualien":                "台湾省",
	"Hubei":                  "湖北省",
	"Hunan":                  "湖南省",
	"Inner Mongolia":         "内蒙古",
	"Islands":                "香港特别行政区",
	"Jiangsu":                "江苏省",
	"Jiangxi":                "江西省",
	"Jilin":                  "吉林省",
	"Kaohsiung":              "台湾省",
	"Keelung":                "台湾省",
	"Kinmen":                 "台湾省",
	"Kowloon":                "香港特别行政区",
	"Kowloon City":           "香港特别行政区",
	"Kwai Tsing":             "香港特别行政区",
	"Kwai Tsing District":    "香港特别行政区",
	"Kwun Tong":              "香港特别行政区",
	"Kwun Tong District":     "香港特别行政区",
	"Liaoning":               "辽宁省",
	"Lienchiang":             "台湾省",
	"Macao":                  "澳门特别行政区",
	"Miaoli":                 "台湾省",
	"Nantou":                 "台湾省",
	"Nei Mongol":             "内蒙古",
	"New Taipei":             "台湾省",
	"New Territories":        "香港特别行政区",
	"Ningxia":                "宁夏",
	"Ningxia Huizu":          "宁夏",
	"North":                  "香港特别行政区",
	"North District":         "香港特别行政区",
	"Nossa Senhora do Carmo": "澳门特别行政区",
	"Our Lady of Carmo":      "澳门特别行政区",
	"Penghu":                 "台湾省",
	"Pingtung":               "台湾省",
	"Qinghai":                "青海省",
	"Sai Kung":               "香港特别行政区",
	"Sai Kung District":      "香港特别行政区",
	"Saint Francis Xavier":   "澳门特别行政区",
	"Sha Tin":                "香港特别行政区",
	"Shaanxi":                "陕西省",
	"Sham Shui Po":           "香港特别行政区",
	"Sham Shui Po District":  "香港特别行政区",
	"Shandong":               "山东省",
	"Shanghai":               "上海市",
	"Shanxi":                 "山西省",
	"Sichuan":                "四川省",
	"Southern":               "香港特别行政区",
	"Southern District":      "香港特别行政区",
	"Sé":                     "澳门特别行政区",
	"Tai Po":                 "香港特别行政区",
	"Taichung":               "台湾省",
	"Tainan":                 "台湾省",
	"Taipei":                 "台湾省",
	"Taitung":                "台湾省",
	"Taiwan":                 "台湾省",
	"Taoyuan":                "台湾省",
	"Tianjin":                "天津市",
	"Tibet":                  "西藏",
	"Tsuen Wan":              "香港特别行政区",
	"Tuen Mun":               "香港特别行政区",
	"Wan Chai":               "香港特别行政区",
	"Wong Tai Sin":           "香港特别行政区",
	"Wong Tai Sin District":  "香港特别行政区",
	"Xinjiang":               "新疆",
	"Xinjiang Uygur":         "新疆",
	"Xizang":                 "西藏",
	"Yau Tsim Mong":          "香港特别行政区",
	"Yau Tsim Mong District": "香港特别行政区",
	"Yilan":                  "台湾省",
	"Yuen Long":              "香港特别行政区",
	"Yunlin":                 "台湾省",
	"Yunnan":                 "云南省",
	"Zhejiang":               "浙江省",
}

// ProvinceFromLatin 把拉丁字母的省级地区名归一成本项目的标准写法。
// 认不出时返回 false，调用方应当把省份留空而不是原样存入——存进去会让
// 同一个省因为写法不同被当成两个地区，多源合并与地区限制同时失效。
func ProvinceFromLatin(name string) (string, bool) {
	p, ok := provinceByLatin[strings.TrimSpace(name)]
	return p, ok
}
