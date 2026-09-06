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
// 一个确定的天花板：50 × 24 小时 × 30 天 × 约 100 字节 ≈ 3.6 MB/入站。
const sharingMaxRowsPerHour = 50

type sharingKey struct {
	inboundId int
	ip        string
}

type sharingCell struct {
	province  string
	seconds   int // 本小时累计活跃秒数
	flushedAt int // 上次落库时 seconds 的值
}

// sharingObservation 是一轮采样里的一条「这个 IP 此刻正在实质使用这个入站」。
type sharingObservation struct {
	InboundId int
	IP        string
	Province  string
}

// sharingFlush 是一条待写入的记录。
//
// ActiveSeconds 是本小时的**绝对**活跃秒数而非增量，落库走覆盖式 upsert：
// 绝对值天然幂等——一轮写失败下一轮补上即可，不会因为重试把时长记成两倍。
type sharingFlush struct {
	InboundId     int
	IP            string
	Province      string
	HourStart     int64
	ActiveSeconds int
}

// sharingAccumulator 在内存里累计各来源 IP 的活跃时长，满门槛才产出落库项。
//
// 纯内存、不做持久化恢复：内存累计每满门槛就落一次库，面板重启最多丢当前
// 那一分钟，为这点精度引入一套恢复逻辑不划算。
type sharingAccumulator struct {
	mu    sync.Mutex
	hour  int64
	cells map[sharingKey]*sharingCell
}

func newSharingAccumulator() *sharingAccumulator {
	return &sharingAccumulator{cells: map[sharingKey]*sharingCell{}}
}

// observe 记一轮采样：obs 里的每个 IP 都算作在本轮的 step 秒内持续活跃。
// 返回本轮需要落库的记录（可能为空）。
func (a *sharingAccumulator) observe(now time.Time, obs []sharingObservation, step int) []sharingFlush {
	a.mu.Lock()
	defer a.mu.Unlock()

	hour := model.AlignHourUTC(now)
	var out []sharingFlush
	if hour != a.hour {
		out = a.rolloverLocked(hour)
	}

	perInbound := map[int]int{}
	for k := range a.cells {
		perInbound[k.inboundId]++
	}

	for _, o := range obs {
		key := sharingKey{inboundId: o.InboundId, ip: o.IP}
		cell := a.cells[key]
		if cell == nil {
			// 上限只挡新来源，已在累计的继续累计——否则一次扫描就能把
			// 真实用户从表里挤掉。
			if perInbound[o.InboundId] >= sharingMaxRowsPerHour {
				logger.Warningf("入站 %v 本小时来源 IP 已达 %v 个上限，忽略 %v",
					o.InboundId, sharingMaxRowsPerHour, o.IP)
				continue
			}
			cell = &sharingCell{province: o.Province}
			a.cells[key] = cell
			perInbound[o.InboundId]++
		}
		// 省份以最近一次判定为准：归属地库更新后同一个 IP 的判定可能变，
		// 用新的比留着旧的合理。空串不覆盖已知值——一次查库失败不该把
		// 已经判定出来的省份抹掉。
		if o.Province != "" {
			cell.province = o.Province
		}
		cell.seconds += step
		if cell.seconds-cell.flushedAt >= sharingFlushThreshold {
			cell.flushedAt = cell.seconds
			out = append(out, sharingFlush{
				InboundId: key.inboundId, IP: key.ip, Province: cell.province,
				HourStart: hour, ActiveSeconds: cell.seconds,
			})
		}
	}
	return out
}

// rolloverLocked 结束上一个小时：把已过门槛、但最后一段还没落库的单元补写
// 一次，然后清空。
//
// **不满门槛的余量直接丢弃，不跨小时结转**——结转会让一个每小时只用 30 秒
// 的扫描器攒几轮就攒够门槛落库，正好绕开 sharingFlushThreshold。
func (a *sharingAccumulator) rolloverLocked(newHour int64) []sharingFlush {
	var out []sharingFlush
	for key, cell := range a.cells {
		if cell.seconds >= sharingFlushThreshold && cell.seconds > cell.flushedAt {
			out = append(out, sharingFlush{
				InboundId: key.inboundId, IP: key.ip, Province: cell.province,
				HourStart: a.hour, ActiveSeconds: cell.seconds,
			})
		}
	}
	// 上面遍历的是 map，顺序不定。调用方要把这批依次写进库，排序让同一份
	// 输入永远产生同一个写入次序，测试才好断言。
	sort.Slice(out, func(i, j int) bool {
		if out[i].InboundId != out[j].InboundId {
			return out[i].InboundId < out[j].InboundId
		}
		return out[i].IP < out[j].IP
	})
	a.cells = map[sharingKey]*sharingCell{}
	a.hour = newHour
	return out
}
