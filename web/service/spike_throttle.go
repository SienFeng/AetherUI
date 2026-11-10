package service

import (
	"fmt"
	"sort"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
)

// 流量暴涨自动限速（「面板设置 → 防共享 / 防滥用」，管理员 2026-11-09 选定默认打开）。
//
// 某个入站当天的流量超过「近 14 天日常用量的中位数 × 倍数」、而且不少于下限时，把它限速到当地当天结束：
// 转卖、刷流量会让一个账号的用量突然翻好几倍，而本人的用量有自己的节奏。中位数不被一两天的大下载带偏；
// 历史不足 spikeMinHistoryDays 天的不判——新账号还没有「平时」。只限速、到点自动恢复，不停用、不封禁。
//
// 代价：正常的一次性大下载（系统镜像、游戏）也可能被限速到当天结束。

// spikeMinHistoryDays：过去 14 天里至少这么多天有用量，才有「平时」可比。
const (
	spikeHistoryDays    = 14
	spikeMinHistoryDays = 7
)

type SpikeThrottleService struct {
	settingService SettingService
}

// decideSpike 判断当天用量算不算暴涨，并返回作比较的「平时」（过去有用量那些天的中位数）。纯函数。
func decideSpike(today int64, past []int64, p SpikeThrottleParams) (bool, int64) {
	if !p.Enabled {
		return false, 0
	}
	used := make([]int64, 0, len(past))
	for _, v := range past {
		if v > 0 {
			used = append(used, v)
		}
	}
	if len(used) < spikeMinHistoryDays {
		return false, 0
	}
	sort.Slice(used, func(i, j int) bool { return used[i] < used[j] })
	median := used[len(used)/2]
	if len(used)%2 == 0 {
		median = (used[len(used)/2-1] + used[len(used)/2]) / 2
	}
	threshold := max(p.FloorBytes, int64(p.Multiplier)*median)
	return today >= threshold, median
}

// dailyUsage 返回一个入站当天（当地 0 点起）的用量，与之前 14 天每天的用量（past[0] 是昨天）。
// 当天用小时桶算（日桶当天那一桶要到明天才完整也没关系，但小时桶按当地 0 点切得准）；过去的每一天取日桶与
// 小时桶之和里大的那个：两者各自独立累加同一份增量，哪一级先被保留期清掉都不影响——「用量小时数据保留天数」
// 可以设到 1 天，只看小时桶的话暴涨限速就永远凑不够 7 天的「平时」，而且没有任何提示。
func dailyUsage(inboundId int, now time.Time, loc *time.Location) (int64, []int64, error) {
	tdb := database.GetTrafficDB()
	if tdb == nil {
		return 0, nil, fmt.Errorf("用量库没有打开")
	}
	midnight := time.Unix(model.AlignDay(now, loc), 0).In(loc)
	dayStarts := make([]int64, spikeHistoryDays)
	for d := 1; d <= spikeHistoryDays; d++ {
		// AddDate 按当地日历走：夏令时那一天是 23 或 25 小时，不能按 86400 秒推。
		dayStarts[d-1] = model.AlignDay(midnight.AddDate(0, 0, -d), loc)
	}
	var buckets []model.TrafficBucket
	if err := tdb.Where("granularity = ? AND inbound_id = ? AND bucket_start >= ?",
		model.GranularityHour, inboundId, dayStarts[spikeHistoryDays-1]).Find(&buckets).Error; err != nil {
		return 0, nil, err
	}
	var days []model.TrafficBucket
	if err := tdb.Where("granularity = ? AND inbound_id = ? AND bucket_start >= ? AND bucket_start < ?",
		model.GranularityDay, inboundId, dayStarts[spikeHistoryDays-1], midnight.Unix()).Find(&days).Error; err != nil {
		return 0, nil, err
	}
	index := make(map[int64]int, spikeHistoryDays)
	for i, start := range dayStarts {
		index[start] = i
	}
	today := int64(0)
	past := make([]int64, spikeHistoryDays)
	for _, b := range buckets {
		if b.BucketStart >= midnight.Unix() {
			today += b.Up + b.Down
			continue
		}
		if i, ok := index[model.AlignDay(time.Unix(b.BucketStart, 0), loc)]; ok {
			past[i] += b.Up + b.Down
		}
	}
	for _, b := range days {
		if i, ok := index[b.BucketStart]; ok {
			past[i] = max(past[i], b.Up+b.Down)
		}
	}
	return today, past, nil
}

// Run 跑一轮：删掉到期的限速单（开关关掉时全部撤掉）与不参与暴涨限速的入站的限速单，
// 给当天暴涨的启用入站（不参与的除外）发一张限速到当地当天结束的单。
// 返回限速集合有没有变（限速由 ShapingJob 每 10 秒按 desiredLimits 收敛下去）。
func (s *SpikeThrottleService) Run(now time.Time) (bool, error) {
	db := database.GetDB()
	// 开关读不出时跳过这一轮：GetSpikeThrottleParams 把它按「关闭」处理，那样会把所有生效中的限速单删光。
	// 开关只读这一次、由它决定 Enabled：GetSpikeThrottleParams 会再读一次，那一次碰上瞬时错误同样按「关闭」。
	enable, err := s.settingService.getInt("spikeThrottleEnable")
	if err != nil {
		return false, fmt.Errorf("读取流量暴涨自动限速开关失败，本轮跳过: %w", err)
	}
	p := s.settingService.GetSpikeThrottleParams()
	p.Enabled = enable == 1
	expired := db.Where("until <= ?", now.UnixMilli())
	if !p.Enabled {
		expired = db.Where("1 = 1")
	}
	res := expired.Delete(&model.InboundThrottle{})
	if res.Error != nil {
		return false, res.Error
	}
	changed := res.RowsAffected > 0
	if !p.Enabled {
		return changed, nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return changed, err
	}
	// 管理员改了限速档位：已发出的单跟着改，不按旧档位限到当天结束。
	res = db.Model(&model.InboundThrottle{}).Where("mbps <> ?", p.Mbps).Update("mbps", p.Mbps)
	if res.Error != nil {
		return changed, res.Error
	}
	changed = changed || res.RowsAffected > 0
	var active []model.InboundThrottle
	if err := db.Find(&active).Error; err != nil {
		return changed, err
	}
	throttled := make(map[int]bool, len(active))
	for _, a := range active {
		throttled[a.InboundId] = true
	}
	var inbounds []*model.Inbound
	// 不参与的在 Go 里跳过，不在 SQL 里写 `skip_spike_throttle = false`：老库加列时没有默认值，存量行是 NULL，
	// 那样写会把所有存量入站静默排除在检测之外（TestSpikeThrottleRunTreatsNullExemptColumnAsParticipating）。
	if err := db.Where("enable = ?", true).Order("id asc").Find(&inbounds).Error; err != nil {
		return changed, err
	}
	n := now.In(loc)
	until := time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, loc).UnixMilli()
	for _, in := range inbounds {
		if throttled[in.Id] || in.SkipSpikeThrottle {
			continue
		}
		today, past, err := dailyUsage(in.Id, now, loc)
		if err != nil {
			logger.Warning("流量暴涨检测：读取入站", in.Id, "的用量失败:", err)
			continue
		}
		spike, baseline := decideSpike(today, past, p)
		if !spike {
			continue
		}
		if err := db.Create(&model.InboundThrottle{InboundId: in.Id, Mbps: p.Mbps, Until: until,
			TodayBytes: today, Baseline: baseline, CreatedAt: now.UnixMilli()}).Error; err != nil {
			logger.Warning("流量暴涨检测：入站", in.Id, "限速失败:", err)
			continue
		}
		changed = true
		logger.Warning("流量暴涨自动限速：入站", in.Id, "今天已用", today>>20, "MB，平时每天约", baseline>>20,
			"MB，限速到", p.Mbps, "Mbps 直到当地今天结束")
	}
	// 不参与暴涨限速的入站（Inbound.SkipSpikeThrottle）身上的限速单撤掉。必须放在发单之后：编辑表单打开开关时
	// 在同一个事务里撤过一次，但这一轮的入站列表可能是那之前读的、随后照旧发了单——那次保存早于这里的话，
	// 这里撤；晚于发单的话，保存自己撤。两头都盖住，不留一张要等下一轮（10 分钟）才撤的单。
	// 也兜住别的写入路径（直接改库）。
	res = db.Where("inbound_id IN (?)", db.Model(&model.Inbound{}).Select("id").Where("skip_spike_throttle = ?", true)).
		Delete(&model.InboundThrottle{})
	if res.Error != nil {
		return changed, res.Error
	}
	return changed || res.RowsAffected > 0, nil
}

// ApplySettingsNow 让设置页刚保存的开关与档位立刻作用到生效中的限速单：关掉开关撤掉全部，改了档位改写全部。
// 发新单仍由每 10 分钟一轮的 Run 做——这里只撤、只改，与同时在跑的 Run 不会重复发单。返回限速集合有没有变
// （ShapingJob 10 秒内收敛下去）。开关读不出时什么都不动。
func (s *SpikeThrottleService) ApplySettingsNow() (bool, error) {
	enable, err := s.settingService.getInt("spikeThrottleEnable")
	if err != nil {
		return false, fmt.Errorf("读取流量暴涨自动限速开关失败: %w", err)
	}
	db := database.GetDB()
	if enable != 1 {
		res := db.Where("1 = 1").Delete(&model.InboundThrottle{})
		return res.RowsAffected > 0, res.Error
	}
	mbps := s.settingService.GetSpikeThrottleParams().Mbps
	res := db.Model(&model.InboundThrottle{}).Where("mbps <> ?", mbps).Update("mbps", mbps)
	return res.RowsAffected > 0, res.Error
}

// activeThrottles 返回生效中的暴涨限速（入站 id → Mbps）。
func activeThrottles(now time.Time) (map[int]int, error) {
	var rows []model.InboundThrottle
	if err := database.GetDB().Where("until > ?", now.UnixMilli()).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int]int, len(rows))
	for _, r := range rows {
		out[r.InboundId] = r.Mbps
	}
	return out, nil
}
