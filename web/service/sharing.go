package service

import (
	"net"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/netdiag"
)

// sharingSampleStep 是采样间隔（秒）。**必须与 SharingSampleJob 的 cron
// 表达式一致**：累加器按它折算活跃时长，两边不一致会让记录的时长系统性
// 偏大或偏小，而没有任何一层会报错。
const sharingSampleStep = 30

// sharingWindowDays 是并存判定的窗口。
//
// 够长能看出持续并存，够短不会让三个月前的一次旅游一直挂在告警上。
const sharingWindowDays = 7

// sharingRetentionDays 是行的保留期，比判定窗口长。
//
// 多出来的部分供明细页回溯，判断「一直在共享」还是「上个月出了趟差」。
// 是常量而非设置项——这张表的消费者只有本功能，多一个设置项就要同步改 5 处，
// 漏掉 models.js 会让整个保存配置接口失败，为一份自愈的辅助数据不值得。
const sharingRetentionDays = 30

// sharingAccumulatorInstance 是包级累加器，与 onlineTrackerInstance 同理：
// 采集是跨请求的持续过程，状态不能挂在无状态的 service 上。
var sharingAccumulatorInstance = newSharingAccumulator()

// SharingService 负责共享检测的采集、查询与清理。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type SharingService struct {
	onlineService  OnlineService
	inboundService InboundService
	ipdbService    IPDBService
	settingService SettingService
}

// Sample 采一轮，把到门槛的活跃时长写进库。
//
// 它自己驱动 OnlineService.sample()，**不依赖并发判定**：
// ConcurrencyService.Enforce 在「无人设并发额度且无封禁」时提前返回，一次
// 系统调用都不做（web/service/concurrency.go:108）——把采集挂在它后面的话，
// 检测在最常见的默认配置下会无声失效。sample() 内部有最小采样间隔去重，
// 两条路径共存不会重复读连接表。
func (s *SharingService) Sample(now time.Time) error {
	db := database.GetTrafficDB()
	if db == nil {
		// 库没打开：面板启动时 InitTrafficDB 失败就是这个状态。共享检测
		// 不可用不该让这个任务每 30 秒报一次错。
		return nil
	}
	if !netdiag.Supported {
		return netdiag.ErrUnsupported
	}
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return err
	}
	if err := s.onlineService.sample(); err != nil {
		return err
	}

	idle := time.Duration(sharingSampleStep) * time.Second
	var obs []sharingObservation
	for _, in := range inbounds {
		if !in.Enable {
			continue
		}
		for _, e := range onlineTrackerInstance.snapshotIdle(in.Port, noLocate, idle) {
			// 只收实质活跃的来源。Idle 表示连接还在但这段时间没有任何
			// 字节往来——纯扫描、失败握手不产生字节增长，天然被挡在外面。
			if e.Idle {
				continue
			}
			obs = append(obs, sharingObservation{
				InboundId: in.Id,
				IP:        e.IP,
				Province:  s.provinceOf(e.IP),
			})
		}
	}

	flushes := sharingAccumulatorInstance.observe(now, obs, sharingSampleStep)
	if len(flushes) == 0 {
		return nil
	}
	// 包成一个事务，理由与 TrafficHistoryService.Record 相同：GORM 的
	// SkipDefaultTransaction 默认为 false，不包的话每次 Create 自带一个
	// BEGIN...COMMIT，一轮就是 N 次独立提交，而这块盘还要同时服务主库
	// 和访问日志库。
	return db.Transaction(func(tx *gorm.DB) error {
		for _, f := range flushes {
			if err := upsertIPHour(tx, f); err != nil {
				return err
			}
		}
		return nil
	})
}

// provinceOf 返回主判定省份，查不到时返回空串。
//
// 多个数据源对同一个 IP 可能给出不同省份，取第一个非空的：Sources() 的
// 顺序是固定的，所以同一份库对同一个 IP 永远给出同一个答案。这里不能用
// Multi 的并集语义——那是给地区限制放行用的，而一个 IP 不可能同时属于
// 两个省，并集在这里没有意义。
//
// IPv6 恒返回空串：ipdb 只收录 IPv4（util/ipdb/ipdb.go:64）。
func (s *SharingService) provinceOf(ipStr string) string {
	db := s.ipdbService.DB()
	if db == nil {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	for _, sl := range db.Lookup(ip) {
		if sl.Location.Region != "" {
			return sl.Location.Region
		}
	}
	return ""
}

// upsertIPHour 覆盖式写入一行。
//
// DoUpdates 用覆盖而不是 gorm.Expr 累加（与 upsertBucket 相反）：sharingFlush
// 带的是本小时的**绝对**活跃秒数。覆盖天然幂等——一轮写失败下一轮补上即可，
// 不会因为重试把时长记成两倍。
func upsertIPHour(db *gorm.DB, f sharingFlush) error {
	row := &model.InboundIPHour{
		InboundId: f.InboundId, IP: f.IP, HourStart: f.HourStart,
		Province: f.Province, ActiveSeconds: f.ActiveSeconds,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "inbound_id"}, {Name: "ip"}, {Name: "hour_start"},
		},
		DoUpdates: clause.AssignmentColumns([]string{"province", "active_seconds"}),
	}).Create(row).Error
}

// Cleanup 删除早于保留期的行，返回删除行数。
//
// 条件只有 hour_start 一个——这张表只有一种粒度，不存在 TrafficBucket 那个
// 「不带 granularity 就会把日桶一起删掉」的坑。
func (s *SharingService) Cleanup(now time.Time) (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	cutoff := now.Add(-time.Duration(sharingRetentionDays) * 24 * time.Hour).Unix()
	result := db.Where("hour_start < ?", cutoff).Delete(&model.InboundIPHour{})
	return result.RowsAffected, result.Error
}

// PruneOrphans 删除已不存在的入站遗留的行，返回删除行数。
//
// 这是第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。两道都要有：
// SQLite 会复用被删除的自增 id，残留的行会绑到下一个建出来的入站上，那时
// 引用不再悬空，界面会渲染得非常合理，只是显示的是别人的并存记录。
func (s *SharingService) PruneOrphans() (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	var ids []int
	if err := database.GetDB().Model(model.Inbound{}).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	tx := db.Where("inbound_id != 0")
	if len(ids) > 0 {
		tx = tx.Where("inbound_id not in ?", ids)
	}
	result := tx.Delete(&model.InboundIPHour{})
	return result.RowsAffected, result.Error
}

// DeleteByInbound 删除某入站的全部并存记录。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *SharingService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.InboundIPHour{}).Error
}
