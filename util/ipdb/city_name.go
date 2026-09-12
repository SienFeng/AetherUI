package ipdb

import "strings"

// 本文件把两个数据源的城市写法归一成可比较的形态，与 province_name.go 对省份、
// isp_name.go 对运营商做的事同类，但**落点不同**：省份与运营商是在落库前归一，
// 城市只在分歧判定时归一，库里存的仍是各源的原始写法。
//
// 两条理由决定了这个落点：
//
//  1. 促成 ISP 落库前归一的那个硬约束在城市这里不成立。ISP 有十万级取值、
//     原样入库会撞上格式的 65536 上限；城市只有几百个，怎么存都不会撞。
//  2. 落库前归一要配一个新的格式版本判据才能对存量部署生效——fetchAndBuild
//     的 ETag 条件请求会让上游数据没变的机器一直收 304，而 HasISP() 那类判据
//     看不出城市有没有归一过。还会让 model.InboundIPHour.City 的历史值与新值
//     写法不一，同一个网络族在过渡期显示成横跨多个城市，给共享检测送进一个
//     凭空出现的风险信号。
//
// 不归一的代价是实测出来的：对 bin/ 下两份真实库随机采样 30 万个 IPv4，
// 双库均命中中国段的 27,662 个样本里 87.9% 会亮「存疑」，其中 59.3% 纯粹是
//「南京市」与「南京」差一个字，真正的分歧只有 17.6%。这与 isp_name.go 记的
//「电信」/「中国电信」是同一个坑，只是漏在了城市这一层。

// citySuffixes 是要剥掉的行政建制后缀，长的排在前面：「自治州」必须先于
// 「州」被匹配到，否则民族名会留在结果里（「伊犁哈萨克州」≠「伊犁」）。
var citySuffixes = []string{"特别行政区", "自治州", "自治县", "自治旗", "地区", "盟", "市"}

// ethnicSuffixes 是自治州/自治县名称里夹在地名与「自治州」之间的民族名。
//
// 剥掉「自治州」是不够的：中国的 30 个自治州全称都是「地名 + 民族名 + 自治州」
// （「伊犁哈萨克自治州」），而另一个源给的是纯地名（「伊犁」）。带族称与不带的
// 两种写法都在真实库里出现，不剥就还是两个不同的字符串。
//
// 长的排在前面，且要循环剥：「海西蒙古族藏族自治州」有两个族称。
var ethnicSuffixes = []string{
	"柯尔克孜", "土家族", "布依族", "哈尼族", "傈僳族", "景颇族", "朝鲜族",
	"蒙古族", "哈萨克", "维吾尔", "白族", "傣族", "侗族", "回族", "苗族",
	"羌族", "彝族", "藏族", "壮族", "蒙古",
}

// CanonicalCity 把任意写法的城市名归一成可跨数据源比较的形态。
//
// 它只用于**判断两个源说的是不是同一个地方**，不用于展示：界面上仍然显示各源
// 的原始写法，管理员看到的是数据本来的样子。返回值不保证是任何一种官方写法，
// 只保证同一个城市的不同写法归到同一个串。
//
// 认不出建制后缀的名字原样返回：这个函数宁可漏归一（多报一次存疑）也不能过度
// 归一——把两个真的不同的城市合成一个会让分歧漏报，那时管理员看到的是一个被
// 当成唯一答案的错误归属地，比现在的误报难查得多。
func CanonicalCity(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return ""
	}
	for _, suffix := range citySuffixes {
		trimmed := strings.TrimSuffix(s, suffix)
		if trimmed == s || trimmed == "" {
			continue
		}
		s = trimmed
		// 只有自治建制才可能带族称，普通「市」后面不会再有东西可剥。
		if strings.HasPrefix(suffix, "自治") {
			s = trimEthnic(s)
		}
		break
	}
	return s
}

// trimEthnic 反复剥掉结尾的民族名，直到剥不动为止。剥空则整段保留——
// 一个只由族称构成的名字不是地名，硬剥出空串会让它与任何空城市判成相等。
func trimEthnic(s string) string {
	for {
		matched := false
		for _, e := range ethnicSuffixes {
			trimmed := strings.TrimSuffix(s, e)
			if trimmed == s || trimmed == "" {
				continue
			}
			s = trimmed
			matched = true
			break
		}
		if !matched {
			return s
		}
	}
}
