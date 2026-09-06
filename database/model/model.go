package model

import (
	"fmt"
	"strings"
	"a-ui/util/json_util"
	"a-ui/xray"
)

type Protocol string

const (
	VMess       Protocol = "vmess"
	VLESS       Protocol = "vless"
	Dokodemo    Protocol = "Dokodemo-door"
	Http        Protocol = "http"
	Trojan      Protocol = "trojan"
	Shadowsocks Protocol = "shadowsocks"
)

type User struct {
	Id       int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// 流量自动清零的周期。存的是模式而不是「第几号」：一个入站等于一个用户，
// 管理员心里想的是「这人按自然月算」还是「按他自己的订阅日算」，不是「填几号」。
// 而且订阅日存成快照的话，续期让到期日漂移之后它不会跟着变，名不副实。
const (
	// TrafficResetOff 不自动清零，用量一直累加。
	TrafficResetOff = 0
	// TrafficResetMonthly 每月 1 号 00:00（面板时区）清零。
	TrafficResetMonthly = 1
	// TrafficResetBillCycle 按订阅周期清零：取到期时间那一天的日号，每月同一天
	// 清零；该日在当月不存在时（31 号订阅遇上二月）取当月最后一天。
	// 没有到期时间就没有订阅周期，这个组合会在写入路径被拒绝。
	TrafficResetBillCycle = 2
)

type Inbound struct {
	Id         int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	UserId     int    `json:"-"`
	Up         int64  `json:"up" form:"up"`
	Down       int64  `json:"down" form:"down"`
	Total      int64  `json:"total" form:"total"`
	Remark     string `json:"remark" form:"remark"`
	Enable     bool   `json:"enable" form:"enable"`
	ExpiryTime int64  `json:"expiryTime" form:"expiryTime"`

	// Regions 是允许使用该入站的地区列表，JSON 字符串数组，空表示不限制。
	// 存省级名称（与 util/ipdb 的 Provinces() 一致），不存 CIDR：IP 段会随
	// 归属地库更新而变化，存名称才能在库更新后自动跟着变。
	Regions string `json:"regions" form:"regions"`

	// UpMbit / DownMbit 是该入站的上行 / 下行带宽上限（Mbps），0 表示不限制。
	// 上行 = 客户端发给服务端，下行 = 服务端发给客户端，与 Up/Down 流量统计
	// 同一口径。限速由 Linux 的 tc 实现，见 util/tcshape。
	UpMbit   int `json:"upMbit" form:"upMbit"`
	DownMbit int `json:"downMbit" form:"downMbit"`

	// DisabledByTraffic 记录「这一次停用是 DisableInvalidInbounds 因超流量做的」。
	//
	// 数据库原先不记停用原因，而流量重置要重新启用的只能是这一批：把已过期的
	// 或管理员手动关掉的一并启用，是个静默的错误行为——面板不会有任何提示。
	//
	// 写入方只有两处：DisableInvalidInbounds 因超流量停用时置 true；任何一次
	// 手动写入（UpdateInbound / RenewInbound）与一次成功的重新启用都置 false。
	// 管理员碰过 enable，自动停用的理由就不再成立。
	//
	// json 与 form 都标 "-"：前端能改的话，一个被手动停用的入站可以被伪造成
	// 「因超流量停用」，下个重置周期自己活过来。
	DisabledByTraffic bool `json:"-" form:"-"`

	// TrafficResetMode 是流量自动清零的周期，取值见 TrafficResetOff 等常量。
	//
	// 默认零值即「不重置」，所以 AutoMigrate 给老库加上这一列之后，没有任何
	// 入站会被清零——升级后行为零变化。
	TrafficResetMode int `json:"trafficResetMode" form:"trafficResetMode"`

	// LastResetAt 是上一次自动清零对应的**周期时刻**（毫秒），不是执行时刻。
	// 与 resetInstantFor 算出的本周期时刻比较，决定这一轮要不要清零。
	//
	// json 与 form 都标 "-"：它是服务端状态，既不下发给前端也不接受前端提交。
	// 让浏览器能改它，就等于把「每小时跑一次不会重复清零」这条不变量交出去了
	// ——把它置 0 就能让下一轮任务立刻清一次流量，绕过流量配额。
	LastResetAt int64 `json:"-" form:"-"`

	// ConcurrencyLimit 是同时使用该入站的**不同来源 IP** 数上限，0 表示不限制。
	// 按 IP 而不是按 TCP 连接数计：一个浏览器就会开几十条连接，按连接数设 1
	// 会把用户自己卡死。
	ConcurrencyLimit int `json:"concurrencyLimit" form:"concurrencyLimit"`

	// config part
	Listen         string   `json:"listen" form:"listen"`
	Port           int      `json:"port" form:"port" gorm:"unique"`
	Protocol       Protocol `json:"protocol" form:"protocol"`
	Settings       string   `json:"settings" form:"settings"`
	StreamSettings string   `json:"streamSettings" form:"streamSettings"`
	Tag            string   `json:"tag" form:"tag" gorm:"unique"`
	Sniffing       string   `json:"sniffing" form:"sniffing"`
}

func (i *Inbound) GenXrayInboundConfig() *xray.InboundConfig {
	listen := i.Listen
	// 启用地区限制却没指定监听地址时，限定为纯 IPv4。
	//
	// 实测确认（Xray 26.7.28）：纯 IPv4 的允许集配上 ext:file:!TAG 取反，
	// 遇到 IPv6 来源会 **放行**——匹配器不会拿 v6 地址去比对一个没有 v6
	// 条目的集合。让客户端根本连不上 IPv6 是主手段；路由里那条 ::/0 的
	// 拒绝规则是纵深防御，防的是管理员把监听地址改回留空。
	if listen == "" && HasRegions(i.Regions) {
		listen = "0.0.0.0"
	}
	if listen != "" {
		listen = fmt.Sprintf("\"%v\"", listen)
	}
	return &xray.InboundConfig{
		Listen:         json_util.RawMessage(listen),
		Port:           i.Port,
		Protocol:       string(i.Protocol),
		Settings:       json_util.RawMessage(i.Settings),
		StreamSettings: json_util.RawMessage(i.StreamSettings),
		Tag:            i.Tag,
		Sniffing:       json_util.RawMessage(i.Sniffing),
	}
}

// HasRegions 判断地区限制是否启用。这里只做最轻量的判断，不解析 JSON：
// 真正的解析与校验在 service.DecodeRegions，数据损坏时那边会让整份配置
// 生成失败，而不是在这里静默当成「不限制」。
func HasRegions(encoded string) bool {
	trimmed := strings.TrimSpace(encoded)
	return trimmed != "" && trimmed != "null" && trimmed != "[]"
}

type Setting struct {
	Id    int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Key   string `json:"key" form:"key"`
	Value string `json:"value" form:"value"`
}
