package model

// IPBan 是对某入站上某个来源 IP 的封禁。
//
// 它与并发额度是两回事：额度判定每轮重算、幂等收敛，被踢的客户端下一秒
// 重连、额度够了就照样放行——这正是「踢下线」对用户毫无感觉的原因。封禁
// 是显式的、带时限的状态，在有效期内每一轮都会把该 IP 的连接断掉。
//
// 存的是 inbound_id 外键，而 SQLite 会复用被删除的自增 id，因此入站被删除时
// 必须连带清掉它的封禁（InboundService.DelInbound），否则下一个建出来的入站
// 会凭空继承上一个用户的封禁名单。
type IPBan struct {
	Id        int    `json:"id" gorm:"primaryKey;autoIncrement"`
	InboundId int    `json:"inboundId" gorm:"index:idx_ipban_inbound_ip,unique"`
	IP        string `json:"ip" gorm:"index:idx_ipban_inbound_ip,unique"`

	// ExpiresAt 是到期时间（毫秒）。**0 表示永久**，不会自动解除。
	ExpiresAt int64 `json:"expiresAt"`
	CreatedAt int64 `json:"createdAt"`
}

// Active 判断封禁在 nowMillis 这一刻是否仍然生效。
func (b *IPBan) Active(nowMillis int64) bool {
	return b.ExpiresAt == 0 || b.ExpiresAt > nowMillis
}
