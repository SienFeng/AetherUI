package service

import (
	"sort"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/domain"
)

const (
	// meterOutboundBudget 是全部入站共用的计量出站总预算。
	//
	// 上限来自实测（spec §6.0）：每个计量出站给配置解析加约 1.03 ms、给配置
	// 加约 235 B。这条代价的落点不是 xray 启动（多花 0.2 秒无人察觉），而是
	// routing_validate.go 那套**同步 HTTP 请求里的真实 xray 校验**——新建/编辑
	// 出站节点、新建/编辑入站、保存设置各会 exec 一次 run -test，200 个计量
	// 出站意味着这些操作各多花约 0.21 秒。
	meterOutboundBudget = 200

	// meterPoolCapMax 是单个入站的池容量上限。单入站场景不必把预算吃满：
	// 60 个注册域名已能覆盖绝大多数用户流量的主体。
	meterPoolCapMax = 60

	// meterPoolRound 是「一轮」的长度，必须与 MeterPoolJob 的周期一致。
	meterPoolRound = time.Hour

	// meterPoolWindow 是排序时回看多久的数据。
	meterPoolWindow = 24 * time.Hour

	// meterMinHoldRounds 是最小驻留轮数：刚进池的域名还没来得及产生字节，
	// 权重必然是 0，不保护就会被自己的 0 权重挤出去，永远拿不到实测数据。
	meterMinHoldRounds = 2

	// meterProbeGiveUpRounds 是连续几轮实测字节为 0 就退场。挡的是只有连接数、
	// 没有流量的域名（探测器、失败重连，以及已经被管理员规则分流走的域名）
	// 长期占着槽位。
	meterProbeGiveUpRounds = 3

	// meterCooldown 是退场后多久不再参选。
	meterCooldown = 24 * time.Hour

	// meterSwapMargin 是在位加成，也就是「替换余量」：新候选要挤掉在位者，
	// 权重必须超出它这么多倍。没有它的话，两个权重相近的域名会每轮互换，
	// 出站被反复热增删，而每一次退池都会在核心里留下一对永不回收的计数器。
	meterSwapMargin = 1.25

	// meterStaleCounterLimit 是观测到的死计数器上限，超过即冻结换池。
	//
	// xray 的 RemoveHandler（app/proxyman/outbound/outbound.go:131）只删 handler
	// 不注销 stats 计数器，StatsService 也没有任何注销 RPC，所以每个退过池的
	// tag 会在核心里留到进程退出为止，并且每次 QueryStats 都被返回一遍。
	// 冻结只停止吸纳新域名，已在池内的域名照常计量；xray 任何一次整进程重启
	// 都会清空计数器，观测值归零，冻结自动解除。
	meterStaleCounterLimit = 2000
)

// meterStaleCounters 是上一次采集观测到的「已不在池内的计量计数器」条数。
//
// 观测式而不是记账式：它天然跨 xray 重启自愈，不需要面板去跟踪「上一次重启
// 是什么时候」这种它其实拿不准的状态。写入方是 DomainStatService.RecordMetered
// （每 10 秒一次），读取方是 Recompute（每小时一次）。
var meterStaleCounters atomic.Int64

// StaleMeterCounters 返回上一次采集观测到的死计数器条数。
func StaleMeterCounters() int64 { return meterStaleCounters.Load() }

// SetStaleMeterCounters 由采集路径写入。导出是为了让测试能直接构造这个状态。
func SetStaleMeterCounters(n int64) { meterStaleCounters.Store(n) }

// meterAgg 是某入站在窗口内、某个注册域名的聚合结果。
type meterAgg struct {
	Domain string
	Bytes  int64
	Count  int64
}

// meterCandidate 是排序用的候选。Weight 已经乘过在位加成。
type meterCandidate struct {
	Domain   string
	Count    int64
	Weight   float64
	MustKeep bool
}

// meterPoolCapacity 算出单个入站的池容量。
func meterPoolCapacity(enabled int) int {
	if enabled < 1 {
		enabled = 1
	}
	k := meterOutboundBudget / enabled
	if k > meterPoolCapMax {
		k = meterPoolCapMax
	}
	return k
}

// meterAvgBytesPerConn 算出「每次连接平均多少字节」，用来把未计量域名的
// 连接次数折算成与实测字节可比的权重。
//
// 没有任何字节数据时返回 0——此时所有权重都是 0，排序键自然落到第二项
// count desc，也就是「冷启动按访问次数选池」，不需要单独的代码分支。
func meterAvgBytesPerConn(aggs []meterAgg) float64 {
	var bytes, count int64
	for _, a := range aggs {
		// 只统计真的量到过字节的域名。Bytes == 0 的行既可能是「没被计量」
		// 也可能是「计量了但确实没流量」，从聚合结果里分不出来；把它们的
		// 连接数放进分母，就是拿没量过的连接去稀释量过的连接的均值。新装
		// 机器上池里只有极少数域名，这一稀释能到几十倍——而这个估算存在的
		// 唯一理由就是防止池锁死在首日那批域名上，稀释它等于让它静默失效。
		if a.Bytes <= 0 {
			continue
		}
		bytes += a.Bytes
		count += a.Count
	}
	if count <= 0 {
		return 0
	}
	return float64(bytes) / float64(count)
}

// buildMeterCandidates 把聚合结果与当前池状态合成一份排好序的候选列表。
//
// 候选集是「窗口内有数据的域名」∪「当前在池的域名」——后者不能漏：零增量
// 不写行，一个刚进池、窗口内一条 DomainStat 行都没有的域名如果不进候选，
// 就会绕过最小驻留闸门被静默挤出去。
//
// 排序键是 (Weight desc, Count desc, Domain asc)。末位用域名字典序兜底是硬
// 要求：生成期靠这个顺序保证配置逐字节确定。
func buildMeterCandidates(
	aggs []meterAgg,
	inPool map[string]*model.MeterDomain,
	cooling map[string]bool,
	retired map[string]bool,
	now time.Time,
) []meterCandidate {
	avg := meterAvgBytesPerConn(aggs)

	byDomain := make(map[string]meterAgg, len(aggs))
	for _, a := range aggs {
		byDomain[a.Domain] = a
	}
	names := make([]string, 0, len(aggs)+len(inPool))
	for _, a := range aggs {
		names = append(names, a.Domain)
	}
	for d := range inPool {
		if _, ok := byDomain[d]; !ok {
			names = append(names, d)
		}
	}
	// 先定序再构造，SliceStable 的结果才是确定的——map 的遍历顺序是随机的。
	sort.Strings(names)

	cands := make([]meterCandidate, 0, len(names))
	for _, d := range names {
		// 只收真正的注册域名：domain:com 会命中全部 .com，把该入站几乎全部
		// 流量吸进一个计量出站，榜单从此只有一行；IP 字面量则需要 ip 条件，
		// domain 条件对它永不命中，白占一个槽位。
		if !domain.IsRegistrable(d) {
			continue
		}
		if cooling[d] || retired[d] {
			continue
		}
		a := byDomain[d]
		w := float64(a.Bytes)
		if a.Bytes == 0 {
			// 只按实测字节排会让池在第二天就冻死：没进过池的域名字节恒为 0，
			// 永远排在所有已计量域名之后。折算值只用于选谁进池，不写库、
			// 不出现在任何接口返回体里。
			w = float64(a.Count) * avg
		}
		row, pooled := inPool[d]
		mustKeep := false
		if pooled {
			w *= meterSwapMargin
			mustKeep = now.Sub(time.Unix(row.EnteredAt, 0)) < meterMinHoldRounds*meterPoolRound
		}
		cands = append(cands, meterCandidate{Domain: d, Count: a.Count, Weight: w, MustKeep: mustKeep})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Weight != cands[j].Weight {
			return cands[i].Weight > cands[j].Weight
		}
		if cands[i].Count != cands[j].Count {
			return cands[i].Count > cands[j].Count
		}
		return cands[i].Domain < cands[j].Domain
	})
	return cands
}

// pickPool 从排好序的候选里挑出容量为 k 的新池，返回按域名字典序排列的结果。
//
// 两轮：先让处于最小驻留期的占位，再按排序键补足。返回值排序是硬要求，
// 生成期靠它保证配置逐字节确定。
func pickPool(cands []meterCandidate, k int) []string {
	result := make([]string, 0, k)
	seen := make(map[string]bool, k)
	for _, c := range cands {
		if len(result) >= k {
			break
		}
		if !c.MustKeep {
			continue
		}
		result = append(result, c.Domain)
		seen[c.Domain] = true
	}
	for _, c := range cands {
		if len(result) >= k {
			break
		}
		if seen[c.Domain] {
			continue
		}
		result = append(result, c.Domain)
		seen[c.Domain] = true
	}
	sort.Strings(result)
	return result
}

// MeterPoolService 维护计量池：哪些 (入站, 注册域名) 对当前正在被计量。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type MeterPoolService struct {
	settingService SettingService
}

// MeterEntry 是池里的一行，只含生成配置需要的两个字段。
type MeterEntry struct {
	InboundId int
	Domain    string
}

// Pool 返回当前在池的全部条目，按 (inboundId asc, domain asc) 排序。
//
// 排序不是可省的整洁工作：生成期用它产出计量出站与计量规则，而
// Config.Equals 对 OutboundConfigs / RouterConfig 按字节比较——顺序一抖动
// 就恒判不等，那个 10 秒的 cron 会不停重启 xray。
//
// 冷却中的行（CooldownUntil > now）不返回：它们已经退场，留在表里只是为了
// 记住冷却截止时刻。
//
// 用量库不可用时返回空切片而不是报错：整个计量功能自动停用，配置照常生成。
func (s *MeterPoolService) Pool(now time.Time) ([]MeterEntry, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return nil, nil
	}
	var rows []model.MeterDomain
	err := db.Where("cooldown_until <= ?", now.Unix()).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]MeterEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, MeterEntry{InboundId: r.InboundId, Domain: r.Domain})
	}
	// 在 Go 侧排序而不是交给 SQL 的 ORDER BY：排序键是生成期的不变量，
	// 让它和读取方式解耦，将来换查询条件也不会悄悄丢掉顺序。
	sort.Slice(out, func(i, j int) bool {
		if out[i].InboundId != out[j].InboundId {
			return out[i].InboundId < out[j].InboundId
		}
		return out[i].Domain < out[j].Domain
	})
	return out, nil
}

// DeleteByInbound 删除某入站的全部池行。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *MeterPoolService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.MeterDomain{}).Error
}

// PruneOrphans 删除已不存在的入站遗留的池行，返回删除行数。
//
// 第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。两道都要有：
// SQLite 会复用被删除的自增 id，残留行会绑到下一个建出来的入站上，那时
// 引用不再悬空，面板会为一个全新用户生成一批指向别人域名的计量出站与规则，
// 而配置与界面都渲染得完全正常。
func (s *MeterPoolService) PruneOrphans() (int64, error) {
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
	result := tx.Delete(&model.MeterDomain{})
	return result.RowsAffected, result.Error
}

// Recompute 按小时重算全部启用入站的计量池，返回本轮变动的条目数
//（进池 + 退池），0 表示池没有任何变化。
//
// 调用方（MeterPoolJob）只在返回值大于 0 时才置 xray 重启标志：池没变就没有
// 配置改动，白置标志只会让那个 10 秒的消费任务空跑一次。
//
// 用量库不可用时静默返回 0：整个计量功能自动停用，不影响配置生成。
func (s *MeterPoolService) Recompute(now time.Time) (int, error) {
	tdb := database.GetTrafficDB()
	if tdb == nil {
		return 0, nil
	}
	// 死计数器过多时冻结换池。只停止吸纳新域名，已在池内的域名照常计量；
	// xray 任何一次整进程重启都会清空计数器，观测值归零，冻结自动解除。
	if stale := StaleMeterCounters(); stale > meterStaleCounterLimit {
		logger.Warningf("核心里已累积 %v 个已退池的计量计数器（上限 %v），"+
			"暂停调整计量池；xray 下一次整进程重启会清空它们并自动恢复",
			stale, meterStaleCounterLimit)
		return 0, nil
	}
	// 冷却期满的行直接删掉：它们已经不在池里，留着只是为了记冷却时刻，
	// 到期后就是普通的「不在池内」，该重新参选了。
	if err := tdb.Where("cooldown_until > 0 and cooldown_until <= ?", now.Unix()).
		Delete(&model.MeterDomain{}).Error; err != nil {
		return 0, err
	}

	inbounds, err := (&InboundService{}).GetAllInbounds()
	if err != nil {
		return 0, err
	}
	enabled := make([]int, 0, len(inbounds))
	for _, in := range inbounds {
		if in.Enable {
			enabled = append(enabled, in.Id)
		}
	}
	sort.Ints(enabled)
	k := meterPoolCapacity(len(enabled))

	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return 0, err
	}
	since := model.AlignHour(now.Add(-meterPoolWindow), loc)

	changed := 0
	for _, inboundId := range enabled {
		n, err := s.recomputeInbound(tdb, inboundId, k, since, now)
		changed += n
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// recomputeInbound 重算单个入站的池，返回变动条目数。
func (s *MeterPoolService) recomputeInbound(
	tdb *gorm.DB, inboundId, k int, since int64, now time.Time,
) (int, error) {
	var aggs []meterAgg
	err := tdb.Model(&model.DomainStat{}).
		Select("domain, coalesce(sum(up),0) + coalesce(sum(down),0) as bytes, coalesce(sum(count),0) as count").
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?",
			model.GranularityHour, inboundId, since).
		Group("domain").
		Scan(&aggs).Error
	if err != nil {
		return 0, err
	}

	var rows []model.MeterDomain
	if err := tdb.Where("inbound_id = ?", inboundId).Find(&rows).Error; err != nil {
		return 0, err
	}
	inPool := make(map[string]*model.MeterDomain, len(rows))
	cooling := make(map[string]bool)
	for i := range rows {
		r := &rows[i]
		if r.CooldownUntil > now.Unix() {
			cooling[r.Domain] = true
			continue
		}
		inPool[r.Domain] = r
	}

	bytesOf := make(map[string]int64, len(aggs))
	for _, a := range aggs {
		bytesOf[a.Domain] = a.Bytes
	}
	// 在池且本轮实测字节为 0 的，零轮数加一；有字节则清零。
	// 零字节不会在 DomainStat 里留下行，所以这个状态只能由池表自己记。
	zeroRounds := make(map[string]int, len(inPool))
	retired := make(map[string]bool)
	for dom, r := range inPool {
		if bytesOf[dom] > 0 {
			zeroRounds[dom] = 0
			continue
		}
		zeroRounds[dom] = r.ProbeZeroRounds + 1
		if zeroRounds[dom] >= meterProbeGiveUpRounds {
			retired[dom] = true
		}
	}

	cands := buildMeterCandidates(aggs, inPool, cooling, retired, now)
	newPool := pickPool(cands, k)
	inNew := make(map[string]bool, len(newPool))
	for _, d := range newPool {
		inNew[d] = true
	}

	changed := 0
	// 进池与轮数更新。newPool 已按域名排序，写入顺序确定。
	for _, d := range newPool {
		if row, ok := inPool[d]; ok {
			if row.ProbeZeroRounds != zeroRounds[d] {
				if err := tdb.Model(&model.MeterDomain{}).Where("id = ?", row.Id).
					Update("probe_zero_rounds", zeroRounds[d]).Error; err != nil {
					return changed, err
				}
			}
			continue
		}
		if err := tdb.Create(&model.MeterDomain{
			InboundId: inboundId, Domain: d, EnteredAt: now.Unix(),
		}).Error; err != nil {
			return changed, err
		}
		changed++
	}
	// 退池。退场的写冷却、保留行；被更强候选挤掉的直接删。
	leaving := make([]string, 0, len(inPool))
	for d := range inPool {
		if !inNew[d] {
			leaving = append(leaving, d)
		}
	}
	sort.Strings(leaving)
	for _, d := range leaving {
		row := inPool[d]
		if retired[d] {
			err := tdb.Model(&model.MeterDomain{}).Where("id = ?", row.Id).
				Updates(map[string]any{
					"cooldown_until":    now.Add(meterCooldown).Unix(),
					"probe_zero_rounds": 0,
				}).Error
			if err != nil {
				return changed, err
			}
		} else if err := tdb.Delete(&model.MeterDomain{}, row.Id).Error; err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}
