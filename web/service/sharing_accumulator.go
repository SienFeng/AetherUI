package service

import (
	"sort"
	"sync"
	"time"

	"a-ui/database/model"
	"a-ui/logger"
)

// sharingFlushThreshold 是落库门槛（秒）：单个 (入站, IP, 小时) 累计活跃满
// 这么久才写一行，此后每再满这么久更新一次。
//
// 入站端口在公网上会被扫。若每个建立过 TCP 连接的来源都落一行，一次端口
// 扫描就能在一小时内塞进几千行，保留期压不住，明细页也会被垃圾淹没。
// 一次性探测、误连到不了 60 秒，真实使用轻松超过——这道门槛既压行数，也
// 提高信号质量：并存判定要的本就是「两边都在实质使用」。
const sharingFlushThreshold = 60

// sharingMaxRowsPerHour 是单入站单小时的行数上限。
//
// 不是为正常场景设的（正常一小时 2~3 行），是为被针对性刷时让表的大小有
// 一个确定的天花板：50 × 24 小时 × 30 天 × 约 190 字节 ≈ 6.8 MB/入站。
// （190 而不是最初的 100：Country/City/ISP 三个身份快照列后来加了进来。）
const sharingMaxRowsPerHour = 50

type sharingKey struct {
	inboundId int
	ip        string
}

type sharingCell struct {
	meta      NetworkMeta
	seconds   int // 本小时累计活跃秒数
	flushedAt int // 上次确认落库（ack）时 seconds 的值

	// bytes 是本小时累计的上下行字节，upBytes/downBytes 是它的拆分。
	//
	// bytes 独立累加而不是写成 upBytes + downBytes：两者数值恒等，但独立
	// 赋值让「共享检测的输入逐字节不变」这件事在 diff 上看得见，也避免
	// 将来某次重构顺手把 bytes 删掉——它是 ActiveBytes 的唯一来源，而
	// ActiveBytes 是并存判定的门槛判据。
	bytes     int64
	upBytes   int64
	downBytes int64

	// flushedBytes / flushedTime 是上次确认落库时的 bytes 与时刻。只有纯字节
	// 增长（来源在用、但观测时刻恰好没有连接）时，靠它们判断要不要再写一次。
	flushedBytes int64
	flushedTime  time.Time
}

// sharingLoose 是本小时还没有 cell 的来源攒下的纯字节，见 sharingObservation.BytesOnly。
type sharingLoose struct {
	meta     NetworkMeta
	up, down int64
}

// sharingMaxLoosePerHour 是纯字节暂存的来源数上限。它不落库、不占
// sharingMaxRowsPerHour，只是内存里的天花板：扫描器每个探测包都会留下几百
// 字节，暂存表若不封顶，一次分布式扫描就能让它无限增长。
const sharingMaxLoosePerHour = 1024

// sharingObservation 是一轮采样里的一条「这个 IP 此刻正在实质使用这个入站」，
// 或者（BytesOnly 为 true 时）「这个 IP 在这一轮里产生过字节」。
type sharingObservation struct {
	InboundId int
	IP        string
	Meta      NetworkMeta

	// Up/Down 是自上一轮以来的**增量**字节（onlineTracker.drainPending），
	// 含连接表差值与内核销毁通知补上的尾段。
	//
	// 改用增量之前这里给的是 tracker「本次在线期间」的累计值、在 cell 里做
	// 差分：IP 一断开累计值就归零，重连后只要涨回旧值之上，差分就把断开前
	// 那一段吞掉；cell 新建那一轮还只能拿来当基线。两处都是系统性少算。
	Up   int64
	Down int64

	// BytesOnly 为 true 表示这个来源本轮没有被观测为活跃（采样那一刻没有
	// 连接、或被判闲置），只是有字节要记账。它不增加活跃时长；本小时还没有
	// cell 时字节先进暂存，攒满 coexistMinActiveBytes 才建 cell 落库——
	// 既不让短连接为主的客户端整段丢失，也不让扫描探测占用行数上限。
	BytesOnly bool
}

// sharingFlush 是一条待写入的记录。
//
// ActiveSeconds 是本小时的**绝对**活跃秒数而非增量，落库走覆盖式 upsert：
// 绝对值天然幂等——一轮写失败下一轮补上即可，不会因为重试把时长记成两倍。
type sharingFlush struct {
	InboundId     int
	IP            string
	Meta          NetworkMeta
	HourStart     int64
	ActiveSeconds int
	ActiveBytes   int64
	ActiveUp      int64
	ActiveDown    int64
}

// sharingAccumulator 在内存里累计各来源 IP 的活跃时长，满门槛才产出落库项。
//
// 落库是覆盖式的绝对值，所以有两处不能只靠内存：
//   - 面板重启后累加器从零开始，新进程第一次写某一行就会把重启前写下的整小时数据覆盖掉
//     （实测一行从 911 MB / 2370 秒变成 11 MB / 30 秒）。每个小时第一次采样时读一次库里
//     本小时已有的行（seeds），来源在本小时第一次出现时拿它打底。
//   - 写库失败的那一批要重写。产出落库项时不记「已落库」，写库成功、调用方 ack 之后才记；
//     跨小时的收尾写入留在 prev 里每轮重发，直到确认。
type sharingAccumulator struct {
	mu    sync.Mutex
	hour  int64
	cells map[sharingKey]*sharingCell
	loose map[sharingKey]*sharingLoose

	// seeds 是 seedHour 这个小时库里已有、内存里还没有 cell 的行（重启之前那个进程写下的），
	// 已经整理成「已落库」状态的 cell。它们同样占 sharingMaxRowsPerHour 的名额。
	seedHour int64
	seeds    map[sharingKey]*sharingCell

	// prev 是上一个小时（prevHour）的收尾写入里还没确认落库的 cell。只留一个小时：库坏了
	// 一个多小时的话丢掉更早那一小时并告警，内存不随故障时长增长。
	prevHour int64
	prev     map[sharingKey]*sharingCell

	// cappedWarned 记录本小时已经为哪些入站打过上限告警。
	//
	// 不去重的话，一次分布式扫描会让每条被挡下的观测各打一行、每轮采样
	// 重复一遍（500 个来源 ≈ 4 万行/小时），把面板自己的日志一起挤进
	// journald 的限流窗口——而这是一个未认证的远端就能触发的放大。
	cappedWarned map[int]int
}

func newSharingAccumulator() *sharingAccumulator {
	return &sharingAccumulator{
		cells:        map[sharingKey]*sharingCell{},
		loose:        map[sharingKey]*sharingLoose{},
		cappedWarned: map[int]int{},
	}
}

// observe 记一轮采样：obs 里非 BytesOnly 的 IP 都算作在本轮的 step 秒内持续
// 活跃，所有 IP 的增量字节照单全收。返回本轮需要落库的记录（可能为空），其中包括
// 上一小时还没确认落库的收尾写入。写库成功之后调用方必须 ack 这一批。
func (a *sharingAccumulator) observe(now time.Time, obs []sharingObservation, step int) []sharingFlush {
	a.mu.Lock()
	defer a.mu.Unlock()

	hour := model.AlignHourUTC(now)
	if hour != a.hour {
		a.rolloverLocked(hour)
	}
	out := a.pendingPrevLocked()

	perInbound := map[int]int{}
	for k := range a.cells {
		perInbound[k.inboundId]++
	}
	for k := range a.seeds {
		perInbound[k.inboundId]++
	}

	for _, o := range obs {
		key := sharingKey{inboundId: o.InboundId, ip: o.IP}
		cell := a.cells[key]
		if cell == nil && a.seeds[key] != nil {
			// 库里本小时已有这一行（重启前写的）：接着它记。名额已经算过，落库门槛也早就
			// 过了，纯字节观测不用再进暂存。
			cell = a.seeds[key]
			delete(a.seeds, key)
			a.cells[key] = cell
		} else if cell == nil && o.BytesOnly {
			cell = a.stashLooseLocked(key, o, perInbound)
			if cell == nil {
				continue
			}
		} else if cell == nil {
			// 上限只挡新来源，已在累计的继续累计——否则一次扫描就能把
			// 真实用户从表里挤掉。
			if perInbound[o.InboundId] >= sharingMaxRowsPerHour {
				// 不在这里逐条打日志：每 30 秒一轮采样、一次分布式扫描能
				// 让这条 continue 命中几百次，日志汇总留给 rolloverLocked
				// 在跨小时时一次性打印。
				a.cappedWarned[o.InboundId]++
				continue
			}
			cell = &sharingCell{meta: o.Meta}
			// 之前以纯字节身份攒下的部分并进来，不能因为换了条路径就丢掉。
			if l := a.loose[key]; l != nil {
				cell.upBytes, cell.downBytes, cell.bytes = l.up, l.down, l.up+l.down
				delete(a.loose, key)
			}
			a.cells[key] = cell
			perInbound[o.InboundId]++
		}
		cell.upBytes += o.Up
		cell.downBytes += o.Down
		cell.bytes += o.Up + o.Down
		// 画像以最近一次判定为准：归属地库更新后同一个 IP 的判定可能变，
		// 用新的比留着旧的合理。查不到时不覆盖已知值——一次查库失败（库正在
		// 热替换、或这一轮恰好没加载完）不该把已经判定出来的画像抹掉。
		//
		// **整组替换，不逐字段填**：四个字段必须始终来自同一次查询的同一个
		// 数据源，逐字段填会在库更新的那一刻拼出「旧省份 + 新城市」这种跨源
		// 组合，而画像键正是按这一组字段拼的。判据用 HasGeo（Country 非空）
		// 而不是 Province 非空——境外段的 Province 天然为空，按 Province 判
		// 会让境外来源的画像永远停在零值。
		if o.Meta.HasGeo() {
			cell.meta = o.Meta
		}
		if !o.BytesOnly {
			cell.seconds += step
		}
		// 这里不记「已落库」：写库失败的话，下一轮它照样到期、带着最新的绝对值重发。
		if cell.dueForFlush(now) {
			out = append(out, cell.flushOf(key, hour))
		}
	}
	return out
}

// ack 在 flushes 这一批确认落库之后调用，这时才把对应的 cell 记成「已落库」。
//
// 只往前推：采样任务没有防重入（cron 不跳过仍在跑的上一轮），两轮的确认可能乱序到达。
func (a *sharingAccumulator) ack(flushes []sharingFlush, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, f := range flushes {
		key := sharingKey{inboundId: f.InboundId, ip: f.IP}
		switch f.HourStart {
		case a.hour:
			cell := a.cells[key]
			if cell == nil {
				continue
			}
			if f.ActiveSeconds > cell.flushedAt {
				cell.flushedAt = f.ActiveSeconds
			}
			if f.ActiveBytes > cell.flushedBytes {
				cell.flushedBytes = f.ActiveBytes
			}
			if now.After(cell.flushedTime) {
				cell.flushedTime = now
			}
		case a.prevHour:
			// 收尾写入带的是上一小时的终值，写进去的就是 cell 现在的样子才算数。
			if cell := a.prev[key]; cell != nil && cell.seconds == f.ActiveSeconds && cell.bytes == f.ActiveBytes {
				delete(a.prev, key)
			}
		}
	}
}

// needsSeed 报告 hour 这个小时还没读过库里已有的行。
func (a *sharingAccumulator) needsSeed(hour int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seedHour != hour
}

// seed 记下 hour 这个小时库里已有的行，见 sharingAccumulator.seeds。
func (a *sharingAccumulator) seed(hour int64, rows []model.InboundIPHour) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.seedHour = hour
	a.seeds = make(map[sharingKey]*sharingCell, len(rows))
	for _, r := range rows {
		key := sharingKey{inboundId: r.InboundId, ip: r.IP}
		if r.HourStart != hour || (hour == a.hour && a.cells[key] != nil) {
			continue // 内存里已经在记的，以内存为准
		}
		a.seeds[key] = &sharingCell{
			meta:    NetworkMeta{Country: r.Country, Province: r.Province, City: r.City, ISP: r.ISP},
			seconds: r.ActiveSeconds, flushedAt: r.ActiveSeconds,
			bytes: r.ActiveBytes, upBytes: r.ActiveUp, downBytes: r.ActiveDown,
			flushedBytes: r.ActiveBytes,
		}
	}
}

// pendingPrevLocked 返回上一小时还没确认落库的收尾写入。
func (a *sharingAccumulator) pendingPrevLocked() []sharingFlush {
	var out []sharingFlush
	for key, cell := range a.prev {
		out = append(out, cell.flushOf(key, a.prevHour))
	}
	// 遍历的是 map，顺序不定。调用方要把这批依次写进库，排序让同一份输入永远产生
	// 同一个写入次序，测试才好断言。
	sort.Slice(out, func(i, j int) bool {
		if out[i].InboundId != out[j].InboundId {
			return out[i].InboundId < out[j].InboundId
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// stashLooseLocked 处理一条本小时还没有 cell 的纯字节观测。
//
// 加上本轮字节后攒满 coexistMinActiveBytes、且行数上限还有余量时，返回一个
// 带着此前暂存字节的新 cell（本轮字节由调用方照常累加）；否则把本轮字节记进
// 暂存并返回 nil。暂存表满时丢弃新来源，理由与 sharingMaxRowsPerHour 相同。
func (a *sharingAccumulator) stashLooseLocked(key sharingKey, o sharingObservation, perInbound map[int]int) *sharingCell {
	l := a.loose[key]
	var have int64
	if l != nil {
		have = l.up + l.down
	}
	// 先判门槛、再判暂存有没有空位：暂存表被一次扫描塞满时，单次就超过
	// 门槛的新来源仍然要能落库，否则这一小时里新的短连接用户全被静默丢掉。
	if have+o.Up+o.Down >= coexistMinActiveBytes && perInbound[key.inboundId] < sharingMaxRowsPerHour {
		cell := &sharingCell{meta: o.Meta}
		if l != nil {
			if !o.Meta.HasGeo() {
				cell.meta = l.meta
			}
			cell.upBytes, cell.downBytes, cell.bytes = l.up, l.down, have
			delete(a.loose, key)
		}
		a.cells[key] = cell
		perInbound[key.inboundId]++
		return cell
	}
	if l == nil {
		if o.Up+o.Down == 0 || len(a.loose) >= sharingMaxLoosePerHour {
			return nil
		}
		l = &sharingLoose{}
		a.loose[key] = l
	}
	if o.Meta.HasGeo() {
		l.meta = o.Meta
	}
	l.up += o.Up
	l.down += o.Down
	return nil
}

// eligible 判断这个 cell 够不够格落库：活跃满 sharingFlushThreshold 秒，或
// 字节满 coexistMinActiveBytes。后者与并存判定的「实质使用」是同一道门槛，
// 扫描探测到不了。
func (c *sharingCell) eligible() bool {
	return c.seconds >= sharingFlushThreshold || c.bytes >= coexistMinActiveBytes
}

// dueForFlush 判断本轮要不要写库。按秒的节奏与改动前一致（每多活跃
// sharingFlushThreshold 秒写一次）；只有字节在涨时，距上次写库也至少要隔
// 同样的秒数，免得纯字节来源每 30 秒写一次。
func (c *sharingCell) dueForFlush(now time.Time) bool {
	if !c.eligible() {
		return false
	}
	if c.seconds-c.flushedAt >= sharingFlushThreshold {
		return true
	}
	if c.bytes == c.flushedBytes {
		return false
	}
	return c.flushedTime.IsZero() || now.Sub(c.flushedTime) >= sharingFlushThreshold*time.Second
}

func (c *sharingCell) flushOf(key sharingKey, hour int64) sharingFlush {
	return sharingFlush{
		InboundId: key.inboundId, IP: key.ip, Meta: c.meta,
		HourStart: hour, ActiveSeconds: c.seconds, ActiveBytes: c.bytes,
		ActiveUp: c.upBytes, ActiveDown: c.downBytes,
	}
}

// rolloverLocked 结束上一个小时：已过门槛、但最后一段还没落库的单元转进 prev，
// 收尾写入由 observe 每轮发出、直到确认落库，然后清空。
//
// **不满门槛的余量直接丢弃，不跨小时结转**——结转会让一个每小时只用 30 秒
// 的扫描器攒几轮就攒够门槛落库，正好绕开 sharingFlushThreshold。
func (a *sharingAccumulator) rolloverLocked(newHour int64) {
	if len(a.prev) > 0 {
		logger.Warningf("共享检测：%v 个来源在 %v 那一小时的收尾写入一直没能落库，已放弃",
			len(a.prev), time.Unix(a.prevHour, 0).UTC().Format(time.RFC3339))
	}
	a.prev = map[sharingKey]*sharingCell{}
	for key, cell := range a.cells {
		if cell.eligible() && (cell.seconds > cell.flushedAt || cell.bytes != cell.flushedBytes) {
			a.prev[key] = cell
		}
	}
	a.prevHour = a.hour

	// 每个入站每小时最多打一条汇总告警，而不是每条被挡下的观测各打一行——
	// 遍历 map 打日志是可以的，日志行之间没有顺序契约，不产生任何返回值
	// 里的数组顺序。
	for inboundId, ignored := range a.cappedWarned {
		logger.Warningf("入站 %v 本小时来源 IP 超过 %v 个上限，忽略了 %v 条观测",
			inboundId, sharingMaxRowsPerHour, ignored)
	}
	a.cappedWarned = map[int]int{}

	a.cells = map[sharingKey]*sharingCell{}
	// 不满门槛的纯字节暂存同样不结转，理由同上。
	a.loose = map[sharingKey]*sharingLoose{}
	if a.seedHour != newHour {
		a.seeds = nil
	}
	a.hour = newHour
}

// forget 丢掉某入站在内存里的全部累计。
//
// 必须在删除入站时调用：只删库不清内存的话，rolloverLocked 会在下一次
// 跨小时时把残量重新 upsert 回去，而 PruneOrphans 每小时才跑一次。
// SQLite 会复用被删除的自增 id，那批复活的行会绑到下一个建出来的入站上，
// 那时引用不再悬空，兜底清理也拦不住它们。
func (a *sharingAccumulator) forget(inboundId int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for key := range a.cells {
		if key.inboundId == inboundId {
			delete(a.cells, key)
		}
	}
	// 暂存不落库，但它可能在本小时内攒满门槛升格成 cell，同样要清。
	for key := range a.loose {
		if key.inboundId == inboundId {
			delete(a.loose, key)
		}
	}
	// 打底的行与没确认的收尾写入同理：留着的话会被接着记、写回库里。
	for key := range a.seeds {
		if key.inboundId == inboundId {
			delete(a.seeds, key)
		}
	}
	for key := range a.prev {
		if key.inboundId == inboundId {
			delete(a.prev, key)
		}
	}
}
