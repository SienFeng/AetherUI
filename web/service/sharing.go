package service

import (
	"net"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
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
			if !sharingObservable(e) {
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

// sharingObservable 判断一条在线明细是否算作「实质活跃」，可以计入共享
// 检测的采样。
//
//   - Idle 表示连接还在但这段时间没有任何字节往来——纯扫描、失败握手
//     不产生字节增长，天然被挡在外面。
//   - Blocked 是另一条必须挡的路径：snapshotAt 会为「已被并发限制拒绝、
//     连接已断干净」的来源补造条目，让管理员看得见是谁在被挡，而那种
//     条目只设 Blocked、Idle 是零值 false。它们一个字节都没传过，计进来
//     会凭空抬高活跃时长、污染地区建议。
func sharingObservable(e OnlineIP) bool {
	return !e.Idle && !e.Blocked
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
	// 先清内存再删库。反过来的话，两步之间如果正好跨小时，rolloverLocked
	// 会把残量重新写回一批刚被删掉的行。
	sharingAccumulatorInstance.forget(inboundId)

	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.InboundIPHour{}).Error
}

// SharingDetailEntry 是明细里的一条：某个 IP 在某小时的活跃情况。
type SharingDetailEntry struct {
	IP            string `json:"ip"`
	Province      string `json:"province"`
	ActiveSeconds int    `json:"activeSeconds"`
}

// SharingDetailHour 是明细里的一个小时。
type SharingDetailHour struct {
	HourStart int64 `json:"t"`
	// Label 在**服务端**按面板时区格式化。让浏览器自己格式化的话，访问者
	// 所在时区一变，明细上的时间就和面板设置的时区对不上了——用量图表那边
	// 也是这个理由。
	Label    string               `json:"label"`
	Coexists bool                 `json:"coexists"`
	Entries  []SharingDetailEntry `json:"entries"`
}

// SharingDetail 是某入站的共享检测明细。
type SharingDetail struct {
	Stat       CoexistStat      `json:"stat"`
	Suggestion RegionSuggestion `json:"suggestion"`
	// Hours 按时间倒序，只含发生过并存的小时——全都列出来的话，一个正常
	// 用户 30 天有几百个小时，管理员要找的那几行会被淹掉。
	Hours []SharingDetailHour `json:"hours"`
	// WindowDays 与 RetentionDays 下发给前端做文案，避免两边各写一份常量
	// 然后慢慢漂移。
	WindowDays    int `json:"windowDays"`
	RetentionDays int `json:"retentionDays"`
}

// windowRows 读某入站在给定天数内的行。inboundId 为 0 表示读全部入站。
func (s *SharingService) windowRows(inboundId, days int, now time.Time) ([]model.InboundIPHour, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return nil, nil
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).Unix()
	tx := db.Where("hour_start >= ?", cutoff)
	if inboundId != 0 {
		tx = tx.Where("inbound_id = ?", inboundId)
	}
	var rows []model.InboundIPHour
	err := tx.Order("hour_start asc, ip asc").Find(&rows).Error
	return rows, err
}

// Summary 返回各入站的并存统计，只含达到显示下限的。
//
// 低于下限的不返回：那是旅游迁移交界处的噪声，报出来会让入站列表变成
// 满屏黄标，告警就此失去意义。
func (s *SharingService) Summary(now time.Time) (map[int]CoexistStat, error) {
	rows, err := s.windowRows(0, sharingWindowDays, now)
	if err != nil {
		return nil, err
	}
	byInbound := map[int][]model.InboundIPHour{}
	for _, r := range rows {
		byInbound[r.InboundId] = append(byInbound[r.InboundId], r)
	}
	out := map[int]CoexistStat{}
	for id, list := range byInbound {
		if stat := computeCoexist(list); stat.Flagged() {
			out[id] = stat
		}
	}
	return out, nil
}

// Detail 返回某入站的明细：判定窗口内的统计与建议，加上保留期内的并存时段。
//
// inboundId 必须为正：windowRows 把 0 当作「读全部入站」的哨兵值，那是给
// Summary 用的内部约定。不挡住的话，一个 /sharing/detail/0 的请求会静默
// 返回跨所有入站聚合出来的统计与建议，而界面上看不出任何异常——与本项目
// 一贯要防的「哨兵值让范围被静默放大」是同一类缺陷。
func (s *SharingService) Detail(inboundId int, now time.Time) (*SharingDetail, error) {
	if inboundId <= 0 {
		return nil, common.NewError("入站 id 非法:", inboundId)
	}
	statRows, err := s.windowRows(inboundId, sharingWindowDays, now)
	if err != nil {
		return nil, err
	}
	// 明细比判定窗口看得更远：多出来的那段是用来判断「一直在共享」还是
	// 「上个月出了趟差」的。
	historyRows, err := s.windowRows(inboundId, sharingRetentionDays, now)
	if err != nil {
		return nil, err
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}

	detail := &SharingDetail{
		Stat:          computeCoexist(statRows),
		Suggestion:    suggestRegions(statRows),
		Hours:         []SharingDetailHour{},
		WindowDays:    sharingWindowDays,
		RetentionDays: sharingRetentionDays,
	}

	byHour := map[int64][]model.InboundIPHour{}
	order := make([]int64, 0)
	for _, r := range historyRows {
		if _, seen := byHour[r.HourStart]; !seen {
			order = append(order, r.HourStart)
		}
		byHour[r.HourStart] = append(byHour[r.HourStart], r)
	}
	// historyRows 已按 hour_start 升序，这里倒过来给前端：最近的排最前。
	for i := len(order) - 1; i >= 0; i-- {
		hour := order[i]
		list := byHour[hour]
		// 只列发生过并存的小时。全都列出来的话，一个正常用户 30 天有几百
		// 个小时，管理员要找的那几行会被淹掉。
		if computeCoexist(list).Hours == 0 {
			continue
		}
		entries := make([]SharingDetailEntry, 0, len(list))
		for _, r := range list {
			entries = append(entries, SharingDetailEntry{
				IP: r.IP, Province: r.Province, ActiveSeconds: r.ActiveSeconds,
			})
		}
		detail.Hours = append(detail.Hours, SharingDetailHour{
			HourStart: hour,
			Label:     time.Unix(hour, 0).In(loc).Format("2006-01-02 15:04"),
			Coexists:  true,
			Entries:   entries,
		})
	}
	return detail, nil
}
