package service

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/ipdb"
	"a-ui/util/qqwry"
)

const (
	ip2regionKey  = "ip2region"
	ip2regionPath = "bin/ipdb.dat"

	qqwryKey  = "qqwry"
	qqwryPath = "bin/ipdb-qqwry.dat"

	// 下载被中途截断时，生成出来的库是「合法但残缺」的：Parse 能通过，段数却少了一截，
	// 于是省份 CIDR 集合悄悄变小，地区限制开始误拒合法用户。用段数下限挡住它。
	// 实测 ip2region 约 21.6 万段、纯真约 26 万段，这个下限留了足够余量。
	minValidSegments = 100000

	// ip2region 源数据 35 MB，慢速网络下需要留足时间。
	ipdbDownloadTimeout = 10 * time.Minute

	// 纯真库要整个读进内存才能解析（它靠文件内的绝对偏移互相引用，没法流式处理）。
	// 实测约 25 MB，这个上限防的是下载地址被换成一个巨大文件把内存撑爆。
	qqwryMaxSourceBytes = 64 << 20
)

// ipdbSource 描述一个离线归属地数据源。
//
// 多源并存的理由：实测 ip2region 与纯真对同一批 IP 的判定互有出入，谁也不是
// 权威。两个源一起用既能交叉校验，也能在某个源的下载地址失效时兜底。
type ipdbSource struct {
	Key         string
	Name        string
	Path        string
	MinSegments int
	// URL 为空表示该源未启用。
	URL   func(s *SettingService) (string, error)
	Build func(r io.Reader, w io.Writer, builtAt time.Time) error
}

// ipdbSourceList 的顺序是固定的，它决定 Lookup 的返回顺序与界面展示顺序。
//
// 做成变量而不是函数，是本包唯一一处为可测性开的口子：定时更新的调度逻辑
// （到点判断、单源失败不拖累另一源）必须能测，而真实的源指向 bin/ 下的固定
// 路径和几十 MB 的外部地址，测试里必须能换掉。
var ipdbSourceList = func() []ipdbSource {
	return []ipdbSource{
		{
			Key: ip2regionKey, Name: "ip2region", Path: ip2regionPath,
			MinSegments: minValidSegments,
			URL:         func(s *SettingService) (string, error) { return s.GetIPDBSourceUrl() },
			Build:       ipdb.Build,
		},
		{
			Key: qqwryKey, Name: "纯真 IP 库", Path: qqwryPath,
			MinSegments: minValidSegments,
			URL:         func(s *SettingService) (string, error) { return s.GetQQWrySourceUrl() },
			Build:       buildQQWry,
		},
	}
}

// buildQQWry 把 qqwry.dat 转成本项目的紧凑格式。
func buildQQWry(r io.Reader, w io.Writer, builtAt time.Time) error {
	data, err := io.ReadAll(io.LimitReader(r, qqwryMaxSourceBytes+1))
	if err != nil {
		return err
	}
	if len(data) > qqwryMaxSourceBytes {
		return common.NewErrorf("纯真库源数据超过 %v 字节上限，疑似地址有误", qqwryMaxSourceBytes)
	}
	records, err := qqwry.Parse(data)
	if err != nil {
		return err
	}
	return ipdb.BuildRecords(records, w, builtAt)
}

// IP 库是跨请求共享的只读状态，与本包其它跨请求状态一样放包级变量。
var (
	ipdbLock sync.RWMutex
	ipdbDBs  = map[string]*ipdb.DB{}
)

type IPDBService struct {
	settingService SettingService
}

// GetIPDBPath 返回 ip2region 那一路的库文件路径。
func GetIPDBPath() string { return ip2regionPath }

// DB 返回当前加载的全部数据源。一个都没加载时返回 nil，调用方必须判空——
// 数据文件缺失时面板照常运行，只是归属地与地区限制不可用。
func (s *IPDBService) DB() *ipdb.Multi {
	ipdbLock.RLock()
	defer ipdbLock.RUnlock()

	named := make([]ipdb.Named, 0, len(ipdbDBs))
	for _, src := range ipdbSourceList() {
		if db := ipdbDBs[src.Key]; db != nil {
			named = append(named, ipdb.Named{Key: src.Key, DB: db})
		}
	}
	if len(named) == 0 {
		return nil
	}
	return ipdb.NewMulti(named)
}

func (s *IPDBService) dbOf(key string) *ipdb.DB {
	ipdbLock.RLock()
	defer ipdbLock.RUnlock()
	return ipdbDBs[key]
}

func (s *IPDBService) setDB(key string, db *ipdb.DB) {
	ipdbLock.Lock()
	defer ipdbLock.Unlock()
	ipdbDBs[key] = db
}

// Load 从磁盘加载全部数据源。
//
// 只有一个源都加载不上才算失败：某个源缺失（比如纯真库还没下载过）不影响
// 另一个源继续工作。
func (s *IPDBService) Load() error {
	var lastErr error
	loaded := 0
	for _, src := range ipdbSourceList() {
		db, err := ipdb.Load(src.Path)
		if err != nil {
			lastErr = err
			continue
		}
		s.setDB(src.Key, db)
		loaded++
	}
	if loaded == 0 {
		return lastErr
	}
	return nil
}

// UpdateNow 立刻重建数据源，返回成功的个数。
//
// key 为空表示更新全部；指定 key 只更新那一个——不必为了刷新 2.5 MB 的纯真库
// 把 35 MB 的 ip2region 也重下一遍。
//
// 更新全部时，单个源失败不影响其余源：返回 error 会让一个坏掉的地址把整批
// 更新都挡住。
func (s *IPDBService) UpdateNow(key string) (int, error) {
	sources := ipdbSourceList()
	if key != "" {
		matched := make([]ipdbSource, 0, 1)
		for _, src := range sources {
			if src.Key == key {
				matched = append(matched, src)
			}
		}
		if len(matched) == 0 {
			return 0, common.NewError("未知的 IP 库数据源:", key)
		}
		sources = matched
	}
	updated := 0
	var lastErr error
	for _, src := range sources {
		url, err := src.URL(&s.settingService)
		if err != nil {
			return updated, err
		}
		if url == "" {
			continue
		}
		db, err := s.fetchAndBuild(src, url, src.Path)
		if err != nil {
			lastErr = common.NewError(src.Name+":", err)
			logger.Warning("更新 IP 库失败, 源:", src.Name, "err:", err)
			continue
		}
		s.setDB(src.Key, db)
		updated++
	}
	if updated == 0 && lastErr != nil {
		return 0, lastErr
	}
	return updated, nil
}

// fetchAndBuild 下载源数据、生成 IP 库，校验通过后原子替换目标文件。
//
// 全程写在同目录的临时文件里，只有校验通过才 rename。任何一步失败都绝不能动到
// 已有的库：库过期只是归属地显示不准，而库残缺或为空会让地区限制的取反规则
// 误拒合法用户，集合为空更等于拒绝所有人。
func (s *IPDBService) fetchAndBuild(src ipdbSource, url, dstPath string) (*ipdb.DB, error) {
	client := &http.Client{Timeout: ipdbDownloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, common.NewError("下载 IP 库源数据失败:", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, common.NewErrorf("下载 IP 库源数据失败: HTTP %v", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".ipdb-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	// 失败路径必须清掉临时文件；成功路径上它已被 rename 走，这里的 Remove 会失败，无妨。
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	if err := src.Build(resp.Body, tmp, time.Now().Truncate(time.Second)); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	// 回读一遍：既验证写出来的文件确实能解析，也拿到段数做完整性判断。
	db, err := ipdb.Load(tmpPath)
	if err != nil {
		return nil, common.NewError("生成的 IP 库无法解析:", err)
	}
	if db.SegmentCount() < src.MinSegments {
		return nil, common.NewErrorf("生成的 IP 库只有 %v 段，少于下限 %v，判定为下载不完整",
			db.SegmentCount(), src.MinSegments)
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		return nil, err
	}
	return db, nil
}

// RunScheduledUpdate 供定时任务调用：到点才更新，返回实际更新的源个数。
//
// 用「每天几点」而不是「隔几天」：管理员能把它放到自己的低谷时段，
// 更新时机可预期。更新时刻留空表示关闭自动更新。
func (s *IPDBService) RunScheduledUpdate(now time.Time) (int, error) {
	raw, err := s.settingService.GetIPDBUpdateTime()
	if err != nil {
		return 0, err
	}
	if raw == "" {
		// 关闭时**一次网络请求都不发**，而不是下载完再丢弃：
		// ip2region 源数据 35 MB，白下载既费流量也费 CPU。
		return 0, nil
	}
	at, err := time.Parse("15:04", raw)
	if err != nil {
		return 0, common.NewError("IP 库更新时间格式不正确:", raw, "err:", err)
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return 0, err
	}
	now = now.In(loc)

	updated := 0
	for _, src := range ipdbSourceList() {
		url, err := src.URL(&s.settingService)
		if err != nil {
			return updated, err
		}
		if url == "" {
			continue
		}
		var lastUpdatedAt int64
		if db := s.dbOf(src.Key); db != nil {
			builtAt := db.BuiltAt()
			// 库的时间戳在未来说明机器时钟异常。当成 0 会让它每轮都重下，
			// 这里保持原样交给 ShouldUpdateNow 判断（它会认为今天已经跑过）。
			lastUpdatedAt = builtAt.UnixMilli()
		}
		if !ShouldUpdateNow(now, lastUpdatedAt, at.Hour(), at.Minute()) {
			continue
		}
		db, err := s.fetchAndBuild(src, url, src.Path)
		if err != nil {
			// 单个源失败保留它原来的库，也不影响另一个源。
			logger.Warning("定时更新 IP 库失败, 源:", src.Name, "err:", err)
			continue
		}
		s.setDB(src.Key, db)
		updated++
	}
	return updated, nil
}

// IPDBSourceStatus 是单个数据源的状态，供界面显示。
type IPDBSourceStatus struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
	Loaded        bool   `json:"loaded"`
	BuiltAt       int64  `json:"builtAt"`
	SegmentCount  int    `json:"segmentCount"`
	ProvinceCount int    `json:"provinceCount"`
}

// IPDBStatus 是整个归属地库子系统的状态。
type IPDBStatus struct {
	Sources    []IPDBSourceStatus `json:"sources"`
	UpdateTime string             `json:"updateTime"`
}

func (s *IPDBService) Status() (*IPDBStatus, error) {
	updateTime, err := s.settingService.GetIPDBUpdateTime()
	if err != nil {
		return nil, err
	}
	out := &IPDBStatus{UpdateTime: updateTime, Sources: []IPDBSourceStatus{}}
	for _, src := range ipdbSourceList() {
		url, err := src.URL(&s.settingService)
		if err != nil {
			return nil, err
		}
		st := IPDBSourceStatus{Key: src.Key, Name: src.Name, Enabled: url != ""}
		if db := s.dbOf(src.Key); db != nil {
			st.Loaded = true
			st.BuiltAt = db.BuiltAt().UnixMilli()
			st.SegmentCount = db.SegmentCount()
			st.ProvinceCount = len(db.Provinces())
		}
		out.Sources = append(out.Sources, st)
	}
	return out, nil
}
