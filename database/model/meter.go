package model

import (
	"strconv"
	"strings"
)

// MeterOutboundTagPrefix 是计量出站 tag 的固定前缀。
//
// 计量出站是「给每个被计量的注册域名一个独立 tag」这件事的载体：xray 的
// stats 计数器只有 inbound/outbound/user 三个维度，没有域名维度，所以要
// 拿到按域名的字节数，唯一的办法就是让每个域名拥有自己的出站 tag。
const MeterOutboundTagPrefix = "a-ui-meter-"

// MeterTag 拼出 (入站, 注册域名) 对应的计量出站 tag，形如
// a-ui-meter-3-doubleclick.net。
//
// tag 里直接带域名而不是「槽位序号 + 映射表」，是为了消掉槽位复用的归因
// 错乱：槽位从域名 A 换成 B 时，那一轮采集里属于 A 的残余流量会被算到 B
// 头上，而且没有任何一层会报错。tag 带域名则换池就是换计数器。
func MeterTag(inboundId int, domain string) string {
	return MeterOutboundTagPrefix + strconv.Itoa(inboundId) + "-" + domain
}

// ParseMeterTag 从计量出站 tag 反查出 (入站 id, 注册域名)。
//
// 按**第一个**短横线切开：inboundId 是十进制数字不含短横线，其后全部是
// 域名。域名本身可以含短横线（some-cdn.example.com），所以绝不能从右边切。
//
// 拒绝一切形态不对的输入而不是尽力猜：采集路径上一个猜错的 tag 会把字节
// 静默记到别的域名头上，而榜单会渲染得完全正常。
func ParseMeterTag(tag string) (int, string, bool) {
	rest, ok := strings.CutPrefix(tag, MeterOutboundTagPrefix)
	if !ok {
		return 0, "", false
	}
	idStr, dom, ok := strings.Cut(rest, "-")
	if !ok || idStr == "" || dom == "" {
		return 0, "", false
	}
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		return 0, "", false
	}
	return id, dom, true
}

// IsMeterTag 判断一个 tag 是否由计量子系统发出。
//
// 判的是**前缀**而不是「能否成功反查」：分配端要拒绝的是整个前缀命名空间，
// 一个形态不对但带着前缀的 tag 同样不能分配给出站节点。
func IsMeterTag(tag string) bool {
	return strings.HasPrefix(tag, MeterOutboundTagPrefix)
}
