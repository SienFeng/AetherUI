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

// MeterDomain 是「当前正在被计量」的 (入站, 注册域名) 对，落在用量库
// （见 database.InitTrafficDB），与 DomainStat 同库。
//
// 分库理由与 DomainStat / TrafficBucket 相同：高频写入不该和面板的普通操作
// 抢主库那把 SQLite 写锁；而且这张表的孤儿清理挂在同一个每小时任务里。
//
// 池必须落库，不能在 RoutingInjector.Inject 里现算：Inject 由那个 10 秒的
// 重启消费任务反复调用，现算意味着排名一变配置字节就变，Config.Equals 恒
// 判不等，cron 会不停热应用甚至重启 xray。
//
// 没有任何 json tag：这张表是纯服务端状态，既不下发给前端也不接受前端提交
//（与 model.Inbound 的 LastResetAt / DisabledByTraffic 同一条理由）。
//
// 相应地，删除入站时必须连带删掉它的行——SQLite 会复用被删除的自增 id，
// 不删的话下一个建出来的入站会拿到上一个用户的计量域名，生成出一批指向
// 别人域名的计量出站与规则，而引用不再悬空，跳过式的防线拦不住。
type MeterDomain struct {
	Id int64 `gorm:"primaryKey;autoIncrement"`

	InboundId int    `gorm:"uniqueIndex:idx_meter_domain,priority:1"`
	Domain    string `gorm:"uniqueIndex:idx_meter_domain,priority:2"`

	// EnteredAt 是进池时刻的 Unix 秒，供「最小驻留」闸门使用：刚进池的域名
	// 还没来得及产生字节，它的权重必然是 0，不保护就会被自己的 0 权重挤出去。
	EnteredAt int64

	// ProbeZeroRounds 是进池后连续几轮实测字节为 0。
	//
	// 零字节的域名不会在 DomainStat 里留下行（零增量不写行），所以「在池但
	// 实测为 0」这个状态无法从 DomainStat 反推，必须由池表自己记账——这是
	// 这个字段存在的唯一理由。
	ProbeZeroRounds int

	// CooldownUntil 是冷却截止时刻的 Unix 秒，0 表示不在冷却。
	//
	// 它同时是「这一行是否在池内」的判据：> now 表示已退场、正在冷却，
	// 不生成出站与规则，也不参与本轮候选。行不能直接删掉，否则冷却状态
	// 就没地方存，一个只有连接数没有流量的域名会每轮重新入选、每轮退场。
	CooldownUntil int64
}
