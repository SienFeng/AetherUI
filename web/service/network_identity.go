package service

import "net"

// 网络族（network family）把「同一个网络出来的若干个地址」收成一个键。
//
// 它**不落库**：是 model.InboundIPHour.IP 那一列的纯函数，而那一列已经在库里。
// 存一个派生列没有收益且有害——聚合级别一旦调整，存下来的值当场变错，而且
// 没有任何一层会报错。设计文档 §3.3。
const (
	// networkFamilyV4Bits 是 IPv4 的聚合级别。/24 是同一个接入段的常规粒度，
	// 动态家宽在同一个 /24 内换地址是常态。
	networkFamilyV4Bits = 24

	// networkFamilyV6Bits 是 IPv6 的聚合级别。
	//
	// /64 这个值不是估的，是 RFC 8981（privacy extensions）决定的：主机轮换的
	// 是**后 64 位的接口标识**，前缀不动。所以同一台设备的全部临时地址必然落
	// 在同一个 /64 里，而这正是降级口径误报的主要来源——v6 每设备一地址，
	// 再叠加临时地址轮换，一个人能被数成好几个「来源」。
	//
	// 前缀本身变（PPPoE 重拨、DHCPv6-PD 续约）发生在小时之间，小时内极少；
	// 即便真发生了，结果也只是这一小时数成两个而不是合并成一个——与改动前
	// 按原始地址计数完全一致，不会比现状更差。所以对「这一小时里有几个不同
	// 来源」这个问题，/64 是确定够用的，不需要实测标定。
	//
	// 跨天的画像身份是另一个问题（重拨换前缀会不会把一个人拆成两个画像），
	// 那个需要按真实数据回标，见设计文档 §3.2。届时只需把 /64 再往回截，
	// 不需要重新采集——粗前缀是细前缀的前缀。
	networkFamilyV6Bits = 64
)

// networkFamilyKey 把一个来源地址归到它的网络族，返回 CIDR 形式的键。
//
// 解析不出来时返回空串，**调用方必须回退到原始地址**而不是丢弃这条记录：
// 一条认不出的地址仍然是一个来源，吞掉它会让并存判定凭空少一个来源，
// 那是把误报换成了漏报，方向反了。
func networkFamilyKey(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	bits := networkFamilyV6Bits
	if v4 := ip.To4(); v4 != nil {
		// v4-mapped 的 v6 地址（::ffff:1.2.3.4）在这里折回四字节，与
		// online.normalizeIP 的口径一致：同一个客户端不该因为内核给出的
		// 地址族表示不同而被算成两个来源。
		ip = v4
		bits = networkFamilyV4Bits
	}
	mask := net.CIDRMask(bits, len(ip)*8)
	network := ip.Mask(mask)
	if network == nil {
		return ""
	}
	return (&net.IPNet{IP: network, Mask: mask}).String()
}

// networkFamilyOrIP 是调用方该用的那个：算得出网络族就用族，算不出就用原始地址。
func networkFamilyOrIP(ipStr string) string {
	if key := networkFamilyKey(ipStr); key != "" {
		return key
	}
	return ipStr
}

// NetworkMeta 是某个来源地址在**观测当时**的网络画像快照。
//
// 四个字段同属一个数据源，见 selectNetworkMeta。
type NetworkMeta struct {
	Country  string
	Province string
	City     string
	ISP      string
}

// HasGeo 报告这份快照有没有可用的地理信息。
//
// 主判据是 Country 而不是 Province：ipdb.normalize 对非中国段只保留 Country
// 与知名 IDC 名（util/ipdb/ipdb.go:114-117），境外 IPv4 的 Province 天然为空。
// 只按 Province 判会把整个境外来源群误判成「没有地理信息」，而跨国并存其实
// 完全测得出来。设计文档 §2.1 约束一。
//
// 仍然带上 Province 那一半：生产路径上 Region 非空必然蕴含 Country 非空
// （normalize 只对中国段保留 Region，而中国段的 Country 恒非空），所以这个
// || 在生产上是恒等的冗余；它防的是手工构造的值，以及将来某个只给 Region
// 不给 Country 的数据源——那种输入下「有省份却判成没有地理信息」是纯粹的
// 数据丢失，没有任何一层会报错。
func (m NetworkMeta) HasGeo() bool { return m.Country != "" || m.Province != "" }

// selectNetworkMeta 从多源判定里选出一份快照。
//
// **整组取自同一个源，不跨源拼装。** 多源对同一个 IP 给出不同省份是实测发生
// 过的（v1.23.0 复盘：ip2region 判北京、纯真判湖北，后者才对）；这时若省份
// 取自源 A、城市取自源 B，就会拼出「江苏 + 上海市」这种在界面上看得见的胡话，
// 而画像键正是按这一组字段拼的。
//
// 主源的选法：第一个给出非空 Region 的源；都没有 Region 时退而取第一个给出
// 非空 Country 的源（境外段就是这个形态）。Multi.Sources() 的顺序是固定的，
// 所以同一份库对同一个 IP 永远给出同一个答案。
//
// 代价是：主源若恰好没有 ISP 而另一个源有，这里会丢掉那个 ISP，画像键退化成
// 带网络族的降级形态。那是**信息变少**，不是判断变错——比拼装出一个自洽性
// 可疑的组合好。
func selectNetworkMeta(locs []sourceLocation) NetworkMeta {
	for _, sl := range locs {
		if sl.Region != "" {
			return NetworkMeta{Country: sl.Country, Province: sl.Region, City: sl.City, ISP: sl.ISP}
		}
	}
	for _, sl := range locs {
		if sl.Country != "" {
			return NetworkMeta{Country: sl.Country, City: sl.City, ISP: sl.ISP}
		}
	}
	return NetworkMeta{}
}

// sourceLocation 是 selectNetworkMeta 的输入形状，与 ipdb.Location 同构。
//
// 单独定义一个而不是直接吃 ipdb.SourceLocation：选取逻辑是纯函数，测试不该
// 为了构造几条输入去拼一个带数据源标识的结构。
type sourceLocation struct {
	Country string
	Region  string
	City    string
	ISP     string
}
