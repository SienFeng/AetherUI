package service

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"a-ui/config"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/accesslog"
	"a-ui/util/domain"
	"a-ui/xray"
	"github.com/shirou/gopsutil/disk"
	"gorm.io/gorm"
)

const (
	// accessLogPath 与 bin/config.json 一样是相对路径：xray 进程的工作目录
	// 就是安装根目录（systemd 的 WorkingDirectory）。
	accessLogPath = "bin/access.log"

	// 单轮最多读这么多字节，防止一次把整个日志读进内存。
	accessLogMaxReadBytes = 4 << 20

	// 日志文件超过这个大小就在消费完之后清空。xray 以 O_APPEND 打开，
	// 截断后会继续从 0 追加，不需要重启。
	accessLogMaxFileSize = 16 << 20

	// 剩余磁盘低于这个值就停止写入。日志写满磁盘会让整个面板和 xray
	// 一起失效，代价远大于丢一段日志。
	accessLogMinFreeBytes = 512 << 20

	// 单批插入条数。批太大时 SQLite 的单条语句参数会超限。
	accessLogBatchSize = 200
)

var (
	accessLogTailer     = &accesslog.Tailer{Path: accessLogPath}
	accessLogLock       sync.Mutex
	accessLogDiskWarned bool
	// accessLogResumed 表示本进程已从库里恢复过读取进度（见 model.AccessLogCursor）。
	// 恢复之前一行都不读：恢复失败（例如库被锁）时下一轮重试，而不是从 0 读起再导一遍。
	accessLogResumed bool
)

// injectAccessLog 把访问日志路径合并进 xray 的 log 配置。
//
// 两条约束：
//   - 关闭时**一个字节都不改**。改了会让 Config.Equals 判定配置变化，
//     那个 10 秒的重启消费任务会把所有人踢下线。
//   - 只设置 access 一项，模板里原有的 loglevel / error 等保持不动——
//     那是管理员自己配的。
//
// 用 map 中转再 Marshal：encoding/json 对 map key 排序，生成逐字节确定，
// 否则 Config.Equals 恒为 false，xray 会被那个 cron 反复重启。
func injectAccessLog(cfg *xray.Config, enabled bool, path string) error {
	if !enabled {
		return nil
	}
	fields := map[string]json.RawMessage{}
	if len(cfg.LogConfig) > 0 {
		if err := json.Unmarshal(cfg.LogConfig, &fields); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(path)
	if err != nil {
		return err
	}
	fields["access"] = encoded

	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	cfg.LogConfig = data
	return nil
}

type AccessLogQuery struct {
	InboundId int
	SourceIP  string
	Keyword   string
	Page      int
	PageSize  int

	// SkipTotal 让本次查询不统计总条数，Total 原样返回 0。
	//
	// 给自动刷新用：那条路径每几秒重跑一次同一个查询，而 Count 是其中最贵的
	// 一步——带关键字时条件是 target LIKE '%kw%'，走不了索引，是一次全表扫描，
	// 而访问日志按保留期能攒到百万行。它对「看最新几条」又毫无用处：自动刷新
	// 只在第一页生效，页码和页大小都不会变。
	//
	// 调用方必须知道 Total == 0 在这里表示「没统计」而不是「没有记录」，
	// 因此前端在这条路径上不更新分页总数，沿用上一次手工查询的值。
	SkipTotal bool
}

const (
	accessLogDefaultPageSize = 50
	accessLogMaxPageSize     = 500
)

// Normalize 把越界的分页参数纠正到合法范围。幂等，两条路径都调用它，
// 界面才能显示实际生效的页码与页大小。
func (q *AccessLogQuery) Normalize() {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 || q.PageSize > accessLogMaxPageSize {
		q.PageSize = accessLogDefaultPageSize
	}
}

// AccessLogRow 是一条带归属地的访问记录。
//
// 归属地在查询时判定而不落库：库里已有的历史记录因此立刻都有归属地，IP 库
// 更新之后旧记录的判定也跟着变准，而且与「近期来源」气泡、在线明细共用
// locateWithIPDB，三处不会给出对不上的结论。代价是没法用 SQL 按地区筛选。
//
// 内嵌 model.AccessLog 而不是逐字段复制：JSON 形状原样不变，前端现有的
// row.time / row.sourceIp / row.target 等绑定一个都不用改。
type AccessLogRow struct {
	model.AccessLog
	Location    string `json:"location"`
	LocationAlt string `json:"locationAlt"`
	ISP         string `json:"isp"`
	ISPAlt      string `json:"ispAlt"`
	// Sources 见 ipLocation.Sources：分歧要能追到是哪个源说的。
	Sources []ipSourceLocation `json:"sources"`
	// Evidence 见 ipLocation.Evidence：这个判定有多少源同意。
	Evidence provinceEvidence `json:"evidence"`
}

// AccessLogResult 是查询接口的返回体。
type AccessLogResult struct {
	// Enabled 为 false 时列表多半是空的，界面要提示去设置里打开，
	// 而不是让管理员以为这个人没访问过任何网站。
	Enabled bool `json:"enabled"`
	// Total 在 AccessLogQuery.SkipTotal 为真时恒为 0，表示「本次没统计」，
	// 不是「一条都没有」。见那个字段的说明。
	Total    int64          `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"pageSize"`
	List     []AccessLogRow `json:"list"`
}

type AccessLogService struct {
	settingService SettingService
	ipdbService    IPDBService
}

// Store 把解析出来的记录写进访问日志库，返回写入条数。
func (s *AccessLogService) Store(entries []accesslog.Entry) (int, error) {
	db := database.GetAccessLogDB()
	if db == nil || len(entries) == 0 {
		return 0, nil
	}
	rows, err := accessLogRows(entries)
	if err != nil {
		return 0, err
	}
	if err := db.CreateInBatches(rows, accessLogBatchSize).Error; err != nil {
		return 0, err
	}
	return len(rows), nil
}

// storeWithCursor 把记录与读取进度放在同一个事务里提交：要么日志落库且进度前进，
// 要么都不变（下一轮从原进度重读这一段）。没有记录时只更新进度——整段都是认不出的行
// 也要跳过去，否则每轮都卡在同一处。
func storeWithCursor(db *gorm.DB, entries []accesslog.Entry, cursor model.AccessLogCursor) error {
	rows, err := accessLogRows(entries)
	if err != nil {
		return err
	}
	cursor.Id = 1
	return db.Transaction(func(tx *gorm.DB) error {
		if len(rows) > 0 {
			if err := tx.CreateInBatches(rows, accessLogBatchSize).Error; err != nil {
				return err
			}
		}
		return tx.Save(&cursor).Error
	})
}

// resumeAccessLogTailer 从库里恢复读取进度。没有记录（首次启用）或文件已换过时从头读。
func resumeAccessLogTailer(db *gorm.DB) error {
	var cursor model.AccessLogCursor
	err := db.Where("id = ?", 1).Limit(1).Find(&cursor).Error
	if err != nil {
		return fmt.Errorf("读取访问日志进度失败: %w", err)
	}
	if cursor.Id == 0 {
		return nil
	}
	ok, err := accessLogTailer.Resume(cursor.Offset, cursor.Head)
	if err != nil {
		return fmt.Errorf("恢复访问日志进度失败: %w", err)
	}
	if !ok {
		logger.Info("访问日志文件已更换或被截断过，从文件开头读取")
	}
	return nil
}

func accessLogRows(entries []accesslog.Entry) ([]model.AccessLog, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	tagToId, err := inboundTagToId()
	if err != nil {
		return nil, err
	}

	rows := make([]model.AccessLog, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, model.AccessLog{
			Time:       e.Time.UnixMilli(),
			InboundId:  tagToId[e.Inbound],
			InboundTag: e.Inbound,
			SourceIP:   e.SourceIP,
			Network:    e.Network,
			Target:     e.Target,
			Route:      e.Route,
			Accepted:   e.Accepted,
			NoRuleHit:  e.NoRuleHit,
		})
	}
	return rows, nil
}

func inboundTagToId() (map[string]int, error) {
	var inbounds []*model.Inbound
	if err := database.GetDB().Model(model.Inbound{}).Find(&inbounds).Error; err != nil {
		return nil, err
	}
	m := make(map[string]int, len(inbounds))
	for _, in := range inbounds {
		m[in.Tag] = in.Id
	}
	return m, nil
}

// escapeLike 转义 LIKE 的通配符。不转的话管理员搜一个 "%" 会命中全部记录。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// GetAccessLogs 供接口层调用：校验入站存在、补齐分页、带上启用状态。
func (s *AccessLogService) GetAccessLogs(q AccessLogQuery) (*AccessLogResult, error) {
	inboundService := InboundService{}
	if _, err := inboundService.GetInbound(q.InboundId); err != nil {
		return nil, err
	}
	enabled, err := s.settingService.GetAccessLogEnable()
	if err != nil {
		return nil, err
	}
	q.Normalize()
	list, total, err := s.Query(q)
	if err != nil {
		return nil, err
	}
	// 一页里往往反复出现同一个来源 IP（一个客户端刷一屏是常态），按 IP
	// 记忆化，免得对同一个地址重复做几十次二分查找。
	seen := make(map[string]ipLocation, len(list))
	// make 而不是 var：前端直接拿去渲染表格，不能给 null。
	rows := make([]AccessLogRow, 0, len(list))
	for _, r := range list {
		loc, ok := seen[r.SourceIP]
		if !ok {
			loc = locateWithIPDB(s.ipdbService, net.ParseIP(r.SourceIP))
			seen[r.SourceIP] = loc
		}
		rows = append(rows, AccessLogRow{
			AccessLog:   r,
			Location:    loc.Location,
			LocationAlt: loc.LocationAlt,
			ISP:         loc.ISP,
			ISPAlt:      loc.ISPAlt,
			Sources:     loc.Sources,
			Evidence:    loc.Evidence,
		})
	}
	return &AccessLogResult{
		Enabled:  enabled,
		Total:    total,
		Page:     q.Page,
		PageSize: q.PageSize,
		List:     rows,
	}, nil
}

func (s *AccessLogService) Query(q AccessLogQuery) ([]model.AccessLog, int64, error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return nil, 0, nil
	}
	tx := db.Model(&model.AccessLog{}).Where("inbound_id = ?", q.InboundId)
	if q.SourceIP != "" {
		tx = tx.Where("source_ip = ?", q.SourceIP)
	}
	if q.Keyword != "" {
		tx = tx.Where(`target LIKE ? ESCAPE '\'`, "%"+escapeLike(q.Keyword)+"%")
	}

	var total int64
	if !q.SkipTotal {
		if err := tx.Count(&total).Error; err != nil {
			return nil, 0, err
		}
	}

	q.Normalize()
	page, pageSize := q.Page, q.PageSize

	var list []model.AccessLog
	// 按 id 倒序而不是 time：同一毫秒内的多条记录靠 id 保证次序稳定，
	// 否则翻页时会有记录重复出现或被跳过。
	err := tx.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// Cleanup 删除超过保留期的记录，返回删除条数。
// RecentSource 是从访问日志里聚合出来的一个来源，用于回答「谁来过」。
type RecentSource struct {
	IP          string `json:"ip"`
	Location    string `json:"location"`
	LocationAlt string `json:"locationAlt"`
	ISP         string `json:"isp"`
	ISPAlt      string `json:"ispAlt"`
	// Sources 见 ipLocation.Sources：分歧要能追到是哪个源说的。
	Sources []ipSourceLocation `json:"sources"`
	// Evidence 见 ipLocation.Evidence：这个判定有多少源同意。
	Evidence  provinceEvidence `json:"evidence"`
	FirstSeen int64            `json:"firstSeen"` // 毫秒
	LastSeen  int64            `json:"lastSeen"`
	Count     int64            `json:"count"`
}

// recentSourcesDefaultLimit 是来源列表默认返回的条数。它只是给管理员一个
// 「点进去看日志」的入口，不是分页列表，取太多没有意义。
const recentSourcesDefaultLimit = 20

// RecentSources 聚合某入站近期出现过的来源 IP，按最后出现时间倒序。
//
// 存在的理由：在线明细是瞬时视图，客户端一断开那行就消失，挂在行上的
// 「访问日志」按钮也跟着没了——管理员恰恰在人走之后才最需要复盘。日志本身
// 一直都在库里，缺的只是入口。
//
// 按 inbound_id 过滤天然排除了面板自己查 xray 流量统计留下的记录：那些行
// 的 tag 是 api，库里没有对应入站，inbound_id 落为 0。
func (s *AccessLogService) RecentSources(inboundId int, limit int) ([]RecentSource, error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return []RecentSource{}, nil
	}
	if limit <= 0 {
		limit = recentSourcesDefaultLimit
	}

	var rows []struct {
		SourceIP  string
		FirstSeen int64
		LastSeen  int64
		Cnt       int64
	}
	err := db.Model(&model.AccessLog{}).
		Select("source_ip, min(time) as first_seen, max(time) as last_seen, count(*) as cnt").
		Where("inbound_id = ?", inboundId).
		Group("source_ip").
		Order("last_seen desc").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	list := make([]RecentSource, 0, len(rows))
	for _, r := range rows {
		loc := locateWithIPDB(s.ipdbService, net.ParseIP(r.SourceIP))
		list = append(list, RecentSource{
			IP:          r.SourceIP,
			Location:    loc.Location,
			LocationAlt: loc.LocationAlt,
			ISP:         loc.ISP,
			ISPAlt:      loc.ISPAlt,
			Sources:     loc.Sources,
			Evidence:    loc.Evidence,
			FirstSeen:   r.FirstSeen,
			LastSeen:    r.LastSeen,
			Count:       r.Cnt,
		})
	}
	return list, nil
}

// CountByRoute 统计某入站在 since（Unix **秒**）之后、经由某个出站的连接数。
//
// 给差额分解里「被封禁丢弃」那一行用。只能给出连接数，拿不到字节：
// blackhole 不调用 Dial（proxy/blackhole/blackhole.go 的 Process 对 TCP
// 直接 common.Interrupt(link.Reader)），而出站计数器是包在 Dial 返回的
// 连接上的（app/proxyman/outbound/handler.go 的 getStatCouterConnection），
// 所以那个计数器从来没被装上、恒为 0。这些字节确实发生过、也确实计在
// 入站计数器里，只是无法单独摘出来。
//
// 注意 AccessLog.Time 是**毫秒**，而调用方给的 since 是秒（与 DomainStat
// 的 BucketStart 同单位），换算在这里做——两处单位不一致是这张表的老坑。
func (s *AccessLogService) CountByRoute(inboundId int, route string, since int64) (int64, error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return 0, nil
	}
	var n int64
	err := db.Model(&model.AccessLog{}).
		Where("inbound_id = ? and route = ? and time >= ?", inboundId, route, since*1000).
		Count(&n).Error
	return n, err
}

// TargetRouteCount 是访问日志按「目标 + 出站 + 是否放行 + 是否命中规则」分组后的一行。
type TargetRouteCount struct {
	Target   string
	Route    string
	Accepted bool
	// NoRuleHit 见 model.AccessLog.NoRuleHit。
	NoRuleHit bool
	// SourceIP 只对出站为 a-ui-block 的行填写（其余行为空、不按来源拆分）：判断一条封禁
	// 是不是入站的地区限制造成的，要看来源 IP。只拆这一类行，分组数才不会随来源成倍增长。
	SourceIP string
	Count    int64
	LastAt   int64 // 毫秒
}

// GroupByTargetRoute 统计某入站自 sinceMs（毫秒，与 AccessLog.Time 同单位）以来的
// 日志分组，按次数降序。最多返回 limit 行；多查一行用来判断是否被截断。
// 访问日志库未初始化时返回空。
func (s *AccessLogService) GroupByTargetRoute(inboundId int, sinceMs int64, limit int) (rows []TargetRouteCount, truncated bool, err error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return nil, false, nil
	}
	sourceExpr := "CASE WHEN route = '" + model.BlockOutboundTag + "' THEN source_ip ELSE '' END"
	err = db.Model(&model.AccessLog{}).
		Select("target, route, accepted, no_rule_hit, "+sourceExpr+" as source_ip, count(*) as count, max(time) as last_at").
		Where("inbound_id = ? and time >= ?", inboundId, sinceMs).
		Group("target, route, accepted, no_rule_hit, " + sourceExpr).
		Order("count desc, target, route, source_ip").
		Limit(limit + 1).
		Scan(&rows).Error
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		rows, truncated = rows[:limit], true
	}
	return rows, truncated, nil
}

// MeterableConnCounts 统计某入站自 sinceMs 起「落到了路由规则表末尾」的连接数，按注册域名
// （domain.Registrable，与域名统计同一口径）汇总：没命中任何规则（" >> "）或已经走了计量出站。
//
// 计量规则排在所有规则之后，只接得住本来会走默认出站的流量。被分流规则送去别处的域名
// 连接数再多也永远量不到字节——线上一天 366 次进池里只有 4 次量到过字节，其余都是
// 这类域名在「进池 → 3 轮零字节退场 → 冷却 24 小时 → 再进池」地空转，每次换池都整表
// 热下发一次路由。计量池用这份计数决定谁值得试用。
//
// 访问日志关闭或库不可用时返回 nil（不是空 map）：调用方据此退回只看域名统计的旧口径。
func (s *AccessLogService) MeterableConnCounts(inboundId int, sinceMs int64) (map[string]int64, error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return nil, nil
	}
	enabled, err := s.settingService.GetAccessLogEnable()
	if err != nil || !enabled {
		return nil, err
	}
	var rows []struct {
		Target string
		Count  int64
	}
	err = db.Model(&model.AccessLog{}).
		Select("target, count(*) as count").
		Where("inbound_id = ? and time >= ? and accepted = ? and (no_rule_hit = ? or route like ?)",
			inboundId, sinceMs, true, true, model.MeterOutboundTagPrefix+"%").
		Group("target").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[domain.Registrable(r.Target)] += r.Count
	}
	return out, nil
}

// ReadAfter 按 (time, id) 升序读出游标之后的至多 limit 行，供异常扫描增量读取。
// 条件不单用 id：SQLite 会复用自增 id，日志被清理到 0 行后新行 id 会回到 1，
// 单看 id 会把它们全部漏掉。访问日志库未初始化时返回空。
func (s *AccessLogService) ReadAfter(afterTime, afterId int64, limit int) ([]model.AccessLog, error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return nil, nil
	}
	rows := make([]model.AccessLog, 0)
	err := db.Where("time > ? OR (time = ? AND id > ?)", afterTime, afterTime, afterId).
		Order("time, id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (s *AccessLogService) Cleanup(retentionDays int, now time.Time) (int64, error) {
	db := database.GetAccessLogDB()
	if db == nil || retentionDays <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixMilli()
	result := db.Where("time < ?", cutoff).Delete(&model.AccessLog{})
	return result.RowsAffected, result.Error
}

// DeleteByInbound 删除某入站的全部访问记录。
//
// 必须在删除入站时调用：SQLite 会复用被删除的自增 id，不清掉的话，
// 下一个建出来的入站会看到上一个用户的访问记录。
func (s *AccessLogService) DeleteByInbound(inboundId int) error {
	db := database.GetAccessLogDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.AccessLog{}).Error
}

// PruneOrphans 删除已不存在的入站留下的访问记录，返回删除条数。
//
// DeleteByInbound 是主路径，这里是兜底：删除入站时若日志库恰好打不开，
// 记录会留下来；等 SQLite 把那个自增 id 分配给新入站，新用户就会看到
// 上一个人的访问记录。这条每小时跑一次的清理让那种状态能自愈。
//
// inbound_id = 0 的记录不动：它们写入时就没匹配上任何入站，界面查不到，
// 交给保留期自然淘汰。
func (s *AccessLogService) PruneOrphans() (int64, error) {
	db := database.GetAccessLogDB()
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
	result := tx.Delete(&model.AccessLog{})
	return result.RowsAffected, result.Error
}

// Collect 消费 xray 写下的新日志行。
func (s *AccessLogService) Collect() error {
	accessLogLock.Lock()
	defer accessLogLock.Unlock()

	enabled, err := s.settingService.GetAccessLogEnable()
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if !hasEnoughDisk() {
		if !accessLogDiskWarned {
			accessLogDiskWarned = true
			logger.Warning("磁盘剩余空间不足, 已暂停写入访问日志")
		}
		return nil
	}
	accessLogDiskWarned = false

	db := database.GetAccessLogDB()
	if db == nil {
		// 访问日志库没初始化成功，没有地方可写：照旧读过即弃，并保住文件大小上限——
		// 不读不截的话 xray 会让 access.log 无限增长。
		if _, err := accessLogTailer.Read(accessLogMaxReadBytes); err != nil {
			return err
		}
		return accessLogTailer.TruncateIfLargerThan(accessLogMaxFileSize)
	}
	if !accessLogResumed {
		if err := resumeAccessLogTailer(db); err != nil {
			return err
		}
		accessLogResumed = true
	}

	lines, next, err := accessLogTailer.Peek(accessLogMaxReadBytes)
	if err != nil {
		return err
	}
	if next != accessLogTailer.Offset() {
		entries := make([]accesslog.Entry, 0, len(lines))
		for _, line := range lines {
			// xray 的 access 文件只写访问记录，但保险起见丢掉认不出的行，
			// 不往库里塞垃圾。
			e, ok := accesslog.ParseLine(line, time.Local)
			// 控制面入站的记录是面板自己每几秒一次的 gRPC 轮询，不是任何用户的流量：
			// 线上一天一万五千多行、约占全表 15%，只白占空间、拖慢查询。
			if !ok || e.Inbound == xray.APIInboundTag {
				continue
			}
			entries = append(entries, e)
		}
		head, err := accessLogTailer.Head()
		if err != nil {
			return err
		}
		// 写库失败时不 Commit：下一轮从原进度重读这一段，不丢也不重。
		if err := storeWithCursor(db, entries, model.AccessLogCursor{Offset: next, Head: head}); err != nil {
			return err
		}
		accessLogTailer.Commit(next)
	}
	return accessLogTailer.TruncateIfLargerThan(accessLogMaxFileSize)
}

// hasEnoughDisk 判断日志库所在分区是否还有余量。取不到用量时放行——
// 探测本身出问题不该把功能关掉。
func hasEnoughDisk() bool {
	usage, err := disk.Usage(filepath.Dir(config.GetAccessLogDBPath()))
	if err != nil || usage == nil {
		return true
	}
	return usage.Free >= accessLogMinFreeBytes
}
