package service

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"a-ui/config"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/accesslog"
	"a-ui/xray"
	"github.com/shirou/gopsutil/disk"
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

// AccessLogResult 是查询接口的返回体。
type AccessLogResult struct {
	// Enabled 为 false 时列表多半是空的，界面要提示去设置里打开，
	// 而不是让管理员以为这个人没访问过任何网站。
	Enabled  bool              `json:"enabled"`
	Total    int64             `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"pageSize"`
	List     []model.AccessLog `json:"list"`
}

type AccessLogService struct {
	settingService SettingService
}

// Store 把解析出来的记录写进访问日志库，返回写入条数。
func (s *AccessLogService) Store(entries []accesslog.Entry) (int, error) {
	db := database.GetAccessLogDB()
	if db == nil || len(entries) == 0 {
		return 0, nil
	}
	tagToId, err := inboundTagToId()
	if err != nil {
		return 0, err
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
		})
	}
	if err := db.CreateInBatches(rows, accessLogBatchSize).Error; err != nil {
		return 0, err
	}
	return len(rows), nil
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
	if list == nil {
		// 前端直接拿去渲染表格，不能给 null。
		list = []model.AccessLog{}
	}
	return &AccessLogResult{
		Enabled:  enabled,
		Total:    total,
		Page:     q.Page,
		PageSize: q.PageSize,
		List:     list,
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
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
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

	lines, err := accessLogTailer.Read(accessLogMaxReadBytes)
	if err != nil {
		return err
	}
	entries := make([]accesslog.Entry, 0, len(lines))
	for _, line := range lines {
		// xray 的 access 文件只写访问记录，但保险起见丢掉认不出的行，
		// 不往库里塞垃圾。
		if e, ok := accesslog.ParseLine(line, time.Local); ok {
			entries = append(entries, e)
		}
	}
	if _, err := s.Store(entries); err != nil {
		return err
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
