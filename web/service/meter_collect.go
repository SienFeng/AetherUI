package service

import (
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/xray"
)

// meterDelta 是一轮采集里某个 (入站, 域名) 的字节增量。
type meterDelta struct {
	inboundId int
	domain    string
	up        int64
	down      int64
}

// RecordMetered 把计量出站的字节增量归到域名分时桶上。
//
// 输入必须是 XrayTrafficJob 那**同一次** GetTraffic(reset=true) 的结果：
// reset=true 会清零 xray 侧的计数器，另起一次独立拉取会让两条链路互相偷数据。
//
// 与 TrafficHistoryService.Record 并列挂在 AddTraffic 上，失败只告警不阻断：
// inbounds.up/down 是限额与到期判定的输入，它停止累加的后果（用户超额不被
// 停用）比榜单少一段字节数据严重得多。
//
// 一个必须记住的语义差异：DomainStat.Count 来自访问日志，记的是**连接建立**
// 时刻；Up/Down 来自这里，记的是**流量发生**时刻。一条 10:59 建立、传到 11:30
// 的连接，它的 1 次计数落在 10 点桶，字节分落在 10 点和 11 点两个桶。所以同一
// 个桶里两者不是同一批连接的统计量，一个域名完全可能出现 Count=0 而 Up=5GB
// 的行。把它们强行对齐需要连接级的字节数，而 xray 不提供（spec §2）。
func (s *DomainStatService) RecordMetered(traffics []*xray.Traffic, now time.Time) error {
	db := database.GetTrafficDB()
	if db == nil || len(traffics) == 0 {
		return nil
	}
	// 先扫一遍。一个没开计量的面板每 10 秒也会收到一整份全零的增量，
	// 提前返回能省掉下面的池查询与 GetTimeLocation（后者每次都要重新读
	// tzdata 文件，Go 不缓存它）。
	//
	// 归零观测值也必须做在这里：xray 整进程重启之后计数器全清，这一支正是
	// 那之后的第一轮，观测值不跟着归零的话冻结永远解不开。
	hasMeter := false
	for _, t := range traffics {
		if !t.IsInbound && model.IsMeterTag(t.Tag) {
			hasMeter = true
			break
		}
	}
	if !hasMeter {
		SetStaleMeterCounters(0)
		return nil
	}

	pool, err := (&MeterPoolService{}).Pool(now)
	if err != nil {
		return err
	}
	inPool := make(map[string]bool, len(pool))
	for _, e := range pool {
		inPool[model.MeterTag(e.InboundId, e.Domain)] = true
	}

	var stale int64
	byKey := make(map[string]*meterDelta, len(traffics))
	for _, t := range traffics {
		if t.IsInbound || !model.IsMeterTag(t.Tag) {
			continue
		}
		// 观测死计数器：xray 的 RemoveHandler（app/proxyman/outbound/outbound.go:131）
		// 只删 handler 不注销 stats 计数器，StatsService 也没有注销 RPC，
		// 所以退过池的 tag 会留到进程退出为止，每轮都被返回一遍（值为 0）。
		// 数出来供 MeterPoolService.Recompute 决定要不要冻结换池。
		if !inPool[t.Tag] {
			stale++
		}
		// 零增量不写行，与 TrafficBucket 同规。上面那些死计数器返回的正是 0，
		// 不跳过的话每轮都会为它们造一堆空桶。
		if t.Up == 0 && t.Down == 0 {
			continue
		}
		id, dom, ok := model.ParseMeterTag(t.Tag)
		if !ok {
			// 形态不对的 tag 无法归因，硬猜只会把字节记到错的域名上。
			logger.Debug("跳过无法解析的计量出站 tag:", t.Tag)
			continue
		}
		d := byKey[t.Tag]
		if d == nil {
			d = &meterDelta{inboundId: id, domain: dom}
			byKey[t.Tag] = d
		}
		d.up += t.Up
		d.down += t.Down
	}
	SetStaleMeterCounters(stale)
	if len(byKey) == 0 {
		return nil
	}

	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return err
	}
	hour := model.AlignHour(now, loc)
	day := model.AlignDay(now, loc)

	// 按 tag 排序后再写：map 的遍历顺序是随机的，固定写序让并发下的锁顺序
	// 也固定，且失败时重跑的行为可复现。
	tags := make([]string, 0, len(byKey))
	for tag := range byKey {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	return db.Transaction(func(tx *gorm.DB) error {
		for _, tag := range tags {
			d := byKey[tag]
			if err := upsertDomainStatBytes(tx, model.GranularityHour, d, hour); err != nil {
				return err
			}
			if err := upsertDomainStatBytes(tx, model.GranularityDay, d, day); err != nil {
				return err
			}
		}
		return nil
	})
}

// upsertDomainStatBytes 把字节增量累加进目标桶，桶不存在时创建。
//
// 与第一期的 upsertDomainStat 完全同构，只是改的列不同：那边累加 count，
// 这边累加 up/down。两者写的可能是同一行——次数来自访问日志聚合，字节来自
// 这里，谁先到谁建行。
func upsertDomainStatBytes(db *gorm.DB, g model.TrafficGranularity, d *meterDelta, start int64) error {
	row := &model.DomainStat{
		Granularity: g, InboundId: d.inboundId, Domain: d.domain,
		BucketStart: start, Up: d.up, Down: d.down,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "granularity"}, {Name: "inbound_id"}, {Name: "domain"}, {Name: "bucket_start"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"up":   gorm.Expr("domain_stats.up + ?", d.up),
			"down": gorm.Expr("domain_stats.down + ?", d.down),
		}),
	}).Create(row).Error
}
