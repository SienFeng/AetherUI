package service

import (
	"time"

	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
)

// IPBanService 管理「踢下线并封禁一段时间」的封禁名单。
//
// 它补的是并发额度判定管不到的场景：额度判定每轮重算、幂等收敛，被踢的
// 客户端下一秒重连、额度够了就照样放行，所以单纯的「踢下线」对用户几乎
// 没有感觉。封禁是显式状态，在有效期内每一轮都会把该 IP 的连接断掉。
type IPBanService struct{}

// Ban 封禁某入站上的一个来源 IP。duration <= 0 表示**永久**封禁。
//
// 同一个 (入站, IP) 只保留一行，重复封禁就地更新到期时间：堆积多行的话
// 解封要删几行才算完，界面上也会出现同一个 IP 的多条记录。
func (s *IPBanService) Ban(inboundId int, ip string, duration time.Duration, now time.Time) error {
	expiresAt := int64(0)
	if duration > 0 {
		expiresAt = now.Add(duration).UnixMilli()
	}
	ban := &model.IPBan{
		InboundId: inboundId,
		IP:        ip,
		ExpiresAt: expiresAt,
		CreatedAt: now.UnixMilli(),
	}
	return database.GetDB().Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "inbound_id"}, {Name: "ip"}},
		DoUpdates: clause.AssignmentColumns([]string{"expires_at", "created_at"}),
	}).Create(ban).Error
}

// Unban 解除某入站上一个 IP 的封禁。目标不存在时不报错——解封是幂等操作，
// 界面上重复点一次不该弹错误。
func (s *IPBanService) Unban(inboundId int, ip string) error {
	return database.GetDB().
		Where("inbound_id = ? and ip = ?", inboundId, ip).
		Delete(&model.IPBan{}).Error
}

// ActiveBans 返回某入站在 now 这一刻仍然生效的封禁，按 IP 升序。
//
// 过期的行不在这里删——判定路径每秒都会走到，顺手做 DELETE 会让每一轮
// 都去抢主库的写锁。清理交给 PruneExpired。
func (s *IPBanService) ActiveBans(inboundId int, now time.Time) ([]*model.IPBan, error) {
	var bans []*model.IPBan
	err := database.GetDB().Model(&model.IPBan{}).
		Where("inbound_id = ? and (expires_at = 0 or expires_at > ?)", inboundId, now.UnixMilli()).
		Order("ip asc").
		Find(&bans).Error
	if err != nil {
		return nil, err
	}
	return bans, nil
}

// DeleteByInbound 清掉某入站的全部封禁，入站被删除时调用。
//
// 必须做：封禁存的是 inbound_id 外键，而 SQLite 会复用被删除的自增 id，
// 不清的话下一个建出来的入站会凭空继承上一个用户的封禁名单。
func (s *IPBanService) DeleteByInbound(inboundId int) error {
	return database.GetDB().
		Where("inbound_id = ?", inboundId).
		Delete(&model.IPBan{}).Error
}

// PruneExpired 删除已过期的封禁行，返回删除条数。永久封禁（expires_at = 0）
// 不受影响。
func (s *IPBanService) PruneExpired(now time.Time) (int64, error) {
	tx := database.GetDB().
		Where("expires_at != 0 and expires_at <= ?", now.UnixMilli()).
		Delete(&model.IPBan{})
	if tx.Error != nil {
		return 0, tx.Error
	}
	return tx.RowsAffected, nil
}

// bannedIPSet 把封禁列表折成一个集合，供每轮判定做 O(1) 查询。
func bannedIPSet(bans []*model.IPBan) map[string]*model.IPBan {
	set := make(map[string]*model.IPBan, len(bans))
	for _, b := range bans {
		set[b.IP] = b
	}
	return set
}
