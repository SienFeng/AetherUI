package ipdb

// MajorityCIDRsOfIDC 返回多数数据源认定为知名机房 / 云厂商（IsKnownIDC）的 IPv4 网段，升序且逐字节确定。
//
// 用途是拦下「在用户的省租一台云服务器转发、绕过地区限制」与「用自己的服务器中转转卖」：真实用户从家宽和
// 手机网络连进来，不会从机房来。判据与 MajorityCIDRsOfProvinces 同一套多数票——支持 = 认为这段是机房的源，
// 反对 = 认为它是别的运营商的源，没有运营商信息的源（旧格式库、境外非机房段）不表态。单个源把一片家宽
// 误标成机房，不能因此把那片用户全拦掉。
//
// 已知局限（2026-11-10 复审，知情接受）：境外段的非机房运营商在建库时就被 CanonicalISP 清空，各源对
// 境外段只能「说是机房」或不表态、投不出反对票；中国段带运营商列的也只有 ip2region 与纯真两个源。所以
// 境外段、以及纯真没收录或给了占位值的中国段，实际是「任一源说是机房就拦」。没有改成「至少 2 个源」：
// 实测本地 ip2region 单独认定约 2.47 亿个地址为机房，旧格式的纯真库（无运营商列）为 0——存量机器的纯真库
// 不重建（IP 库更新时刻默认留空）的话，「至少 2 个源」会让默认打开的机房拦截整个失效。也没有给境外非机房
// 段留标记：画像键（profileKeyOf）把境外段有运营商就当成机房，改了会让共享检测在升级前后断档。被误拦的
// 只可能是没设地区限制的入站（地区限制本来就拦全部境外来源）。
func (m *Multi) MajorityCIDRsOfIDC() []string {
	if len(m.sources) == 0 {
		return nil
	}
	return m.majorityCIDRs(func(loc Location) int {
		switch {
		case loc.ISP == "":
			return 0
		case IsKnownIDC(loc.ISP):
			return 1
		default:
			return -1
		}
	})
}
