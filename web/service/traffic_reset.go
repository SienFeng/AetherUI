package service

import (
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
)

// checkTrafficResetMode 校验重置模式与到期时间的组合。
//
// 「按订阅周期」+ 无到期时间必须在保存时就拒绝，而不是存下来静默地永不生效：
// 管理员选了它、界面上也显示着它，实际却什么都不做，没有任何一层会提示。
func checkTrafficResetMode(in *model.Inbound) error {
	switch in.TrafficResetMode {
	case model.TrafficResetOff, model.TrafficResetMonthly:
		return nil
	case model.TrafficResetBillCycle:
		if in.ExpiryTime <= 0 {
			return common.NewError("按订阅周期重置流量需要先设置到期时间，" +
				"否则没有订阅周期可依据；也可以改选每月 1 号重置或不重置")
		}
		return nil
	default:
		return common.NewErrorf("未知的流量重置周期: %v", in.TrafficResetMode)
	}
}

// resetDayFor 算出这个入站每月第几天重置。第二个返回值为 false 表示它这一轮
// 不参与重置——关闭、模式是脏数据、或按订阅周期却没有到期时间。
//
// 订阅日在运行时从到期时间推导，不存快照：按 30 天续期会让到期日往后漂，
// 存了快照的话重置日不会跟着变，而「订阅周期」这个名字要求它跟着变。
func resetDayFor(in *model.Inbound, loc *time.Location) (int, bool) {
	switch in.TrafficResetMode {
	case model.TrafficResetMonthly:
		return 1, true
	case model.TrafficResetBillCycle:
		if in.ExpiryTime <= 0 {
			return 0, false
		}
		return time.UnixMilli(in.ExpiryTime).In(loc).Day(), true
	default:
		return 0, false
	}
}

// resetInstantFor 返回 now 之前（含当刻）最近的一次「每月 day 号 00:00」的
// 毫秒时间戳，按 loc 对齐。day 超过当月天数时取当月最后一天。
//
// 用「上次重置时刻 vs 本周期重置时刻」比较，而不是判断「今天是不是重置日」，
// 一并解决两件事：任务每小时跑一次，重置日当天不会重复清零；面板停机几天后
// 重启，第一次跑会把漏掉的那一次补上。
func resetInstantFor(now time.Time, day int, loc *time.Location) int64 {
	n := now.In(loc)
	candidate := monthlyInstant(n.Year(), n.Month(), day, loc)
	if candidate.After(n) {
		year, month := previousMonth(n.Year(), n.Month())
		candidate = monthlyInstant(year, month, day, loc)
	}
	return candidate.UnixMilli()
}

// monthlyInstant 返回 year 年 month 月第 day 天的 00:00，day 超过当月天数时
// 取当月最后一天。
//
// 这道钳制不能省，也不能靠 time.Date 自己去归一：31 号订阅的用户在二月会被
// time.Date 归一成 3 月 3 日，重置时刻悄悄跑到下个月去，表征只是「某几个月
// 不清零」，没有任何一层会报错。
func monthlyInstant(year int, month time.Month, day int, loc *time.Location) time.Time {
	if last := daysInMonth(year, month); day > last {
		day = last
	}
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}

// daysInMonth 用「下个月 0 号」拿当月天数，闰年由标准库自己算。
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// previousMonth 显式回退一个月。不能用 AddDate(0, -1, 0)：它作用在已经钳制
// 过的日期上（比如 2 月 28 日）会回到 1 月 28 日，而正确答案是 1 月 31 日。
func previousMonth(year int, month time.Month) (int, time.Month) {
	if month == time.January {
		return year - 1, time.December
	}
	return year, month - 1
}

// ResetDueTraffic 把所有已经越过本周期重置时刻的入站清零，返回清零条数与
// 重新启用条数。
//
// 逐条独立更新而不是一条 UPDATE：每个入站的重置日不同，本周期时刻也就不同，
// 没有一个能写进 SQL 的统一条件。入站数量是几十的量级，不值得为此造一条
// 按 traffic_reset_day 分组的复杂语句。
//
// 写入只更新实际要改的列，不用 Save 写整行：整行写入会把读出来那一刻捕获的
// 快照原样写回，把期间 XrayTrafficJob 刚累加上去的流量静默回滚掉。
func (s *InboundService) ResetDueTraffic(now time.Time, loc *time.Location) (int, int, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	if err := db.Model(model.Inbound{}).
		Where("traffic_reset_mode > 0").Find(&inbounds).Error; err != nil {
		return 0, 0, err
	}

	reset, reEnabled := 0, 0
	for _, in := range inbounds {
		// 写入路径已经挡住非法组合，这里再拦一道是为了直接改库、或将来某条
		// 新的写入路径漏掉校验留下的脏数据：宁可不重置让管理员察觉，也不能
		// 猜一个日号去清零。
		day, ok := resetDayFor(in, loc)
		if !ok {
			logger.Warning("跳过入站", in.Tag,
				"的流量重置：重置周期无法确定（模式", in.TrafficResetMode, "）")
			continue
		}
		instant := resetInstantFor(now, day, loc)
		if in.LastResetAt >= instant {
			continue
		}
		fields := map[string]any{"up": 0, "down": 0, "last_reset_at": instant}
		// 只把「因超流量被自动停用、且还没到期」的那一批拉回来。已过期的
		// 不动，也不清标记：管理员续期时 RenewInbound 会一并处理。
		reviving := in.DisabledByTraffic && !in.Enable && !expiredAt(in, now)
		if reviving {
			fields["enable"] = true
			fields["disabled_by_traffic"] = false
		}
		if err := db.Model(model.Inbound{}).
			Where("id = ?", in.Id).UpdateColumns(fields).Error; err != nil {
			return reset, reEnabled, err
		}
		reset++
		if reviving {
			reEnabled++
		}
	}
	return reset, reEnabled, nil
}

// expiredAt 与 DisableInvalidInbounds 的到期判定同口径：0 表示无限期。
func expiredAt(in *model.Inbound, now time.Time) bool {
	return in.ExpiryTime > 0 && in.ExpiryTime <= now.UnixMilli()
}
