package model

import "time"

// InboundIPHour 是某入站的某个来源 IP 在某个 UTC 整点小时内的活跃时长，
// 存在**独立的 SQLite 库**里（与 TrafficBucket 同库，见 database.InitTrafficDB）。
//
// 分库理由与 TrafficBucket 相同：这张表每 30 秒写一批，清理时又是大批量
// DELETE，而 SQLite 一个库只有一把写锁——混在主库里会让面板的每一次普通
// 操作都去和它抢锁。
//
// 它有两个消费者：
//
//  1. 判定「同一小时内，不同省份的 IP 是否在同时使用这个入站」。这个
//     「并存」判据是区分「用户旅游」（位置迁移，不并存）与「节点被转卖」
//     （位置并存）的唯一可靠信号，见设计文档 §1。
//  2. 入站展开行里按来源 IP 的分时段用量（ActiveUp/ActiveDown），见
//     docs/superpowers/specs/2026-09-10-per-ip-usage-window-design.md。
//
// 两个消费者对采集门槛的要求方向相反：并存判定要过滤噪声（60 秒落库门槛、
// 50 IP/小时上限），用量统计希望不漏。冲突以并存判定为准——用量那一侧
// 在界面上标注了口径，见该文档 §2.2 与 §8.2。
type InboundIPHour struct {
	Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

	// InboundId 而不是 tag：入站 tag 是 inbound-<端口> 算出来的，用户改端口
	// tag 就变。相应地，删除入站时必须连带删掉这些行——SQLite 会复用被删除
	// 的自增 id，不删的话下一个建出来的入站会继承上一个用户的并存记录，
	// 而且因为引用不再悬空，任何「跳过悬空引用」式的防线都拦不住它。
	//
	// 除了这条唯一索引，另有两条纯查询索引。它们不是为将来的新功能加的，
	// 修的是这张表从第一天起就有的两个全扫：
	//
	//   idx_iph_inbound_hour (inbound_id, hour_start)
	//     供 SharingService.Detail 与 DeleteByInbound 那类按入站取窗口的查询。
	//     唯一索引顶不上——它的第二列是 ip，`inbound_id = ? AND hour_start >= ?`
	//     只能用上等值的那一段，随后仍要把该入站全部 ip 的条目扫一遍再过滤。
	//
	//   idx_iph_hour (hour_start)
	//     供 Summary 与 Cleanup。Summary **不带入站条件**（它要算全部入站），
	//     而每次加载入站列表都会调它一次；Cleanup 的 `hour_start < ?` 同理。
	//     两者在唯一索引上都用不到任何前缀列，只能全表扫。
	InboundId int    `json:"inboundId" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:1;index:idx_iph_inbound_hour,priority:1"`
	IP        string `json:"ip" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:2"`
	HourStart int64  `json:"t" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:3;index:idx_iph_inbound_hour,priority:2;index:idx_iph_hour"`

	// Province 是主判定省份，空串表示归属地未知（IPv6 来源、归属地库未加载、
	// 或库中查无此段）。空串的行照常入库：IP 维度的并存信息仍有价值，只是
	// 判定会降级成网络族口径（见 service.computeCoexist）。
	Province string `json:"province"`

	// Country / City / ISP 与 Province 一起构成**观测当时**的网络画像快照，
	// 四个字段同属一个数据源（service.networkMetaOf 选定一个主源后整组取值），
	// 不跨源拼装——拼出来的「江苏 + 上海市」这种组合在界面上是看得见的胡话。
	//
	// 为什么要存下来而不是查询时拿当前 ipdb 重算：归属地库会更新，同一个 IP
	// 上个月判江苏、这个月判上海。重算意味着历史事实跟着数据库更新而改变，
	// 而风险评分要拿它当判断依据。这与 Province 当初存下来是同一条理由。
	//
	// 境外段的 City / Province 恒为空：ipdb.normalize 对非中国只保留 Country
	// 与知名 IDC 名（util/ipdb/ipdb.go:114）。所以「Province 为空」不等于
	// 「没有地理信息」，消费侧要按 Country 判断可用性，不能按 Province。
	//
	// 刻意**不存网络族前缀**：它是 IP 那一列的纯函数（service.networkFamilyKey），
	// 存派生列没有收益且有害——聚合级别一旦调整，存下来的值当场变错，而且
	// 没有任何一层会报错。
	Country string `json:"country"`
	City    string `json:"city"`
	ISP     string `json:"isp"`

	// IdentityVersion 标记上面那组快照是哪一版采集逻辑写的。0 = 升级前写入
	// 的行，四个字段恒为空。
	//
	// **绝不用当前 ipdb 给老行补齐**——那不是当时的画像，补出来的是假的历史
	// 事实。消费侧改用连续 Epoch 处理：只分析「最后一个旧版本小时之后」那一段
	// 全部同版本的连续数据，不按行数占比决定时间覆盖，理由见设计文档 §4.2。
	IdentityVersion int `json:"-"`

	ActiveSeconds int `json:"activeSeconds"`

	// ActiveBytes 是本小时该来源 IP 的上下行字节之和，是并存判定的「实质
	// 使用」判据。
	//
	// 为什么不用 ActiveSeconds 承担这个判断：生产实测过一例——某入站显示
	// 「并存 15 小时 / 2 省」，而第二个省那个 IP 15 个小时里每小时只累计
	// 60~180 秒，且在**访问日志里一条记录都没有**（连被路由规则拦下都会留
	// 下 route=a-ui-block 的记录，它连那个都没有）。它只是在反复建连、做
	// TLS 握手，从未发出过一个可路由的请求。活跃时长分不出这种连接与真实
	// 使用，字节量可以：握手/重连是 KB 级，真实使用是 MB 级。
	//
	// 升级前写入的行这一列是 0。computeCoexist 据此整体切换口径而不是逐行
	// 判断，理由见那里。
	ActiveBytes int64 `json:"activeBytes"`

	// ActiveUp/ActiveDown 是本小时该来源 IP 的上行、下行字节。
	//
	// 与 ActiveBytes 并列而不是取代它：ActiveBytes 是并存判定的门槛判据
	// （coexistMinActiveBytes），而升级前写入的行这两个新列恒为 0、
	// ActiveBytes 有值。若把 ActiveBytes 改成由两列相加得出，那批历史行的
	// 判据会当场失效，共享检测的结论会在升级瞬间整体改变，而界面上没有
	// 任何东西说明发生了什么。
	//
	// 升级前的行这两列恒为 0，消费侧据此整批降级（见 service.hasUnsplitBytes），
	// 不逐行判断——理由与 hasActiveBytes 那条完全相同。
	ActiveUp   int64 `json:"activeUp"`
	ActiveDown int64 `json:"activeDown"`
}

// AlignHourUTC 把时刻对齐到它所在 UTC 小时的起点，返回 Unix 秒。
//
// 刻意**不用**面板时区，与 AlignHour 相反。这张表唯一的消费者是「同一
// 小时内是否并存」，该判定只关心两条记录落不落进同一个桶，桶的绝对位置
// 无关——UTC 与本地时区在此完全等价。既然等价，就不该背上 TrafficBucket
// 那个包袱：按本地时区对齐时，管理员改一次时区会让旧桶与重算出的新刻度
// 不相交，历史整段消失。展示时再按面板时区格式化标签即可。
func AlignHourUTC(t time.Time) int64 {
	return t.UTC().Truncate(time.Hour).Unix()
}
