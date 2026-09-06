package service

import (
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
)

const dayMillis int64 = 24 * 60 * 60 * 1000

// maxRenewDays 挡住手误输入的量级错误（比如把 30 敲成 3000000）。
// 预设按钮最长一年，自定义日期走 expiryTime 参数，正常路径都碰不到这个上限。
const maxRenewDays = 3650

// renewedExpiry 计算按天续期后的到期时间。
//
// 未到期则在原到期时间上叠加，保证提前续费不吃亏；已过期或本就无限期
// （old == 0）则从当前时刻起算，避免续了 30 天之后仍然是过期状态。
func renewedExpiry(old, now int64, days int) int64 {
	base := old
	if base <= now {
		base = now
	}
	return base + int64(days)*dayMillis
}

func checkRenewArgs(days int, expiryTime int64) error {
	if days == 0 && expiryTime == 0 {
		return common.NewError("续期参数为空: 需要指定续期天数或到期时间")
	}
	if days != 0 && expiryTime != 0 {
		return common.NewError("续期参数冲突: 续期天数与到期时间只能给一个")
	}
	if days < 0 || days > maxRenewDays {
		return common.NewErrorf("续期天数不合法: %v（应在 1 ~ %v 之间）", days, maxRenewDays)
	}
	if expiryTime < 0 {
		return common.NewError("到期时间不合法:", expiryTime)
	}
	return nil
}

// RenewInbound 给入站续期：days > 0 按天数续，expiryTime > 0 直接指定到期时间，
// 两者必须恰好给一个。
//
// 写入只更新实际要改的列，不用 Save 写整行：整行写入会把 GetInbound 那一刻
// 捕获的快照原样写回，把期间 XrayTrafficJob 刚累加上去的流量静默回滚掉。
func (s *InboundService) RenewInbound(id, days int, expiryTime int64) error {
	if err := checkRenewArgs(days, expiryTime); err != nil {
		return err
	}

	inbound, err := s.GetInbound(id)
	if err != nil {
		return err
	}

	now := time.Now().Unix() * 1000
	newExpiry := expiryTime
	if days > 0 {
		newExpiry = renewedExpiry(inbound.ExpiryTime, now, days)
	}

	// 与 UpdateInbound 同一条规则：管理员手工碰过这条记录，「因超流量被自动
	// 停用」的理由就不再成立。续到过去（等价于立即停用）也要清——否则这条
	// 规则要靠 ResetDueTraffic 里的「已过期」再兜一层，而两者说的是同一件事。
	fields := map[string]any{"expiry_time": newExpiry, "disabled_by_traffic": false}
	// 只有确实续到了未来才重新启用并清零流量。把到期时间手动设到过去等价于
	// 「立即停用」，此时重新启用毫无意义：CheckInboundJob 30 秒后会再把它停用，
	// 中间还白白触发一次 xray 重启。
	if newExpiry > now {
		fields["enable"] = true
		fields["up"] = 0
		fields["down"] = 0
	}

	return database.GetDB().Model(model.Inbound{}).
		Where("id = ?", id).
		UpdateColumns(fields).Error
}
