package service

import (
	"time"

	"a-ui/database/model"
)

// 时间窗口的档位名。与前端 inbounds.html 里 a-radio-button 的 value 一一对应，
// 两边改一处就必须改另一处——认不出的档位会静默回落「今日」，不会报错。
//
// 系统状态页的总览图（/traffic/overview）也走这里，它额外用到 yesterday /
// 24h / 14d / month / lastMonth 几档，与 web/html/xui/index.html 那个时间下拉的选项
// 一一对应。
//
// **注意 Window7d/Window30d 与 traffic_history.go 里的 Range7d/Range30d
// 字符串相同但语义不同**：这里是日历窗口（包含今天在内的 N 个自然日），
// 那里是滑动窗口（从此刻往回数 N×24 小时）。rangeSpec 那套如今只剩
// History / Overview 两个旧函数（及其测试）在用，接口都已改走 ParseWindow。
//
// Window24h 是这里唯一的滑动窗口：「近 24 小时」字面意思就是从此刻往回
// 24 小时，按日历切会在凌晨只剩几个小时的数据。
const (
	WindowToday     = "today"
	WindowYesterday = "yesterday"
	Window24h       = "24h"
	Window3d        = "3d"
	Window7d        = "7d"
	Window14d       = "14d"
	Window30d       = "30d"
	Window1y        = "1y"
	WindowThisMonth = "month"
	WindowLastMonth = "lastMonth"
	WindowCustom    = "custom"
)

// windowMaxSpanDays 是自定义区间的跨度上限。
//
// 区间来自请求体，是不可信输入：一个 1970→2100 的区间会让服务端拉出天量行。
// 366 而不是 365，是为了让「整整一个闰年」这种合理输入不被截断。
const windowMaxSpanDays = 366

// windowHourGranularityDays 是改用日桶的跨度阈值。
//
// 与既有 rangeSpec 的行为一致：30 天用小时桶（720 点），更长用日桶。
const windowHourGranularityDays = 30

// ipUsageMaxWindowDays 是按来源 IP 的分项能覆盖的最大天数。
//
// 等于 sharingRetentionDays——InboundIPHour 只保留这么久，超出的窗口
// 里那部分数据根本不存在。**故意写成独立常量而不是直接引用
// sharingRetentionDays**：两者语义不同（一个是保留期，一个是查询能力
// 上界），将来保留期若改成设置项，这里要跟着改而不是自动漂移。
const ipUsageMaxWindowDays = sharingRetentionDays

// windowDateLayout 是前端传来的自定义日期格式。只收日期不收时刻：
// 时刻由服务端按面板时区补成 0 点与 24 点，浏览器所在时区因此不影响结果。
const windowDateLayout = "2006-01-02"

// TrafficWindow 是一次用量查询的时间范围，左闭右开的 Unix 秒。
//
// 左闭右开而不是闭区间：桶的起点落在 [Start, End) 里就算命中，边界那个桶
// 归属明确，不会被相邻两个窗口各算一次。
type TrafficWindow struct {
	Start       int64
	End         int64
	Granularity model.TrafficGranularity
}

// SpanDays 返回窗口覆盖的天数（向上取整）。
func (w TrafficWindow) SpanDays() int {
	span := w.End - w.Start
	if span <= 0 {
		return 0
	}
	return int((span + 86399) / 86400)
}

// ParseWindow 把档位名或自定义日期翻译成窗口，并完成全部钳制。
//
// 这是不可信输入进入 service 的边界：区间来自请求体，越界的值一律钳制而
// 不是报错——与 rangeSpec 那句「前端传错时给一张能看的图，比报错或空图
// 有用」同一取向。五类钳制：认不出的档位、不可解析的日期、start > end、
// 跨度超上限、End 超过现在。
//
// 档位到区间的翻译放在服务端而不是让前端算时间戳：时区的权威在服务端
// （SettingService.GetTimeLocation），前端算的话，访问者换个时区，同一个
//「今日」就指向不同的绝对时间，而面板设置的时区并没有变。
func ParseWindow(name, startDate, endDate string, loc *time.Location, now time.Time) TrafficWindow {
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	var start, end time.Time
	switch name {
	case WindowCustom:
		s, errS := time.ParseInLocation(windowDateLayout, startDate, loc)
		e, errE := time.ParseInLocation(windowDateLayout, endDate, loc)
		if errS != nil || errE != nil {
			// 钳制二：日期不可解析，回落「今日」。
			start, end = midnight, now
			break
		}
		if s.After(e) {
			// 钳制三：选反了就交换，而不是返回空区间——空区间会让界面
			// 显示一片 0，管理员看不出是自己把日期选反了。
			s, e = e, s
		}
		// 结束日是**包含**的，所以右端取它的次日 0 点。
		start, end = s, e.AddDate(0, 0, 1)
	case WindowYesterday:
		start, end = midnight.AddDate(0, 0, -1), midnight
	case Window24h:
		// 含当前这个小时在内的 24 个整点小时桶（与改动前系统状态页「24 小时」
		// 那一档的 24 个刻度相同）。按面板时区对齐，半小时时区同样正确。
		start, end = time.Unix(model.AlignHour(now, loc), 0).In(loc).Add(-23*time.Hour), now
	case Window3d:
		start, end = midnight.AddDate(0, 0, -2), now
	case Window7d:
		start, end = midnight.AddDate(0, 0, -6), now
	case Window14d:
		start, end = midnight.AddDate(0, 0, -13), now
	case Window30d:
		start, end = midnight.AddDate(0, 0, -29), now
	case WindowThisMonth:
		start, end = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc), now
	case WindowLastMonth:
		// AddDate(0, -1, 0) 作用在每月 1 号上是安全的（1 号不存在「下月没有这一天」
		// 的归一化问题），结果就是上月 1 号。
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		start, end = first.AddDate(0, -1, 0), first
	case Window1y:
		start, end = midnight.AddDate(0, 0, -364), now
	default:
		// 钳制一：认不出的档位（含 WindowToday 本身）回落「今日」。
		start, end = midnight, now
	}

	// 钳制五：右端不得超过现在。选了未来的结束日时，图表不该画出一串
	// 未来的空刻度。
	if end.After(now) {
		end = now
	}
	// 钳制四：跨度超上限时抬起点，而不是拒绝整个请求。
	if maxSpan := time.Duration(windowMaxSpanDays) * 24 * time.Hour; end.Sub(start) > maxSpan {
		start = end.Add(-maxSpan)
	}
	// 交换与钳制之后仍可能出现空区间（结束日在今天之前且起点被抬到它之后
	// 是不可能的，但未来的起始日会落到这里）。空区间统一退回「今日」，
	// 理由同钳制二。
	if !end.After(start) {
		start, end = midnight, now
	}

	g := model.GranularityHour
	if end.Sub(start) > time.Duration(windowHourGranularityDays)*24*time.Hour {
		g = model.GranularityDay
	}

	return TrafficWindow{
		Start:       start.Unix(),
		End:         end.Unix(),
		Granularity: g,
	}
}
