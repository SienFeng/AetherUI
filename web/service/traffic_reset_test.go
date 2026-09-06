package service

import (
	"fmt"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// mustLoad 让用例里的时区名读起来就是它自己，不必每处都摊开错误处理。
func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// makeInboundFor 落一条入站，只要求调用方给出本组测试关心的字段。
// 直接落库、绕开 AddInbound 的校验：本组关心的是重置本身，不是新增流程。
func makeInboundFor(t *testing.T, port int, mutate func(*model.Inbound)) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId:         1,
		Port:           port,
		Protocol:       model.VLESS,
		Tag:            fmt.Sprintf("inbound-%d", port),
		Remark:         fmt.Sprintf("测试-%d", port),
		Enable:         true,
		Settings:       `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811"}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp"}`,
		Sniffing:       `{"enabled":true}`,
	}
	mutate(in)
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("create inbound %d: %v", port, err)
	}
	return in
}

// resetInstantFor 要回答的是「本周期的重置时刻是什么时候」，而不是「今天是不是
// 重置日」。用时刻比较才能同时挡住两件事：每小时跑一次不会在重置日当天重复
// 清零；面板停机几天后重启，第一次跑会把漏掉的那一次补上。
func TestResetInstantForFindsMostRecentResetMoment(t *testing.T) {
	sh := mustLoad(t, "Asia/Shanghai")
	ny := mustLoad(t, "America/New_York")

	cases := []struct {
		name string
		now  time.Time
		day  int
		loc  *time.Location
		want time.Time
	}{
		{
			name: "重置日已过_取本月",
			now:  time.Date(2026, 9, 5, 10, 30, 0, 0, sh),
			day:  1,
			loc:  sh,
			want: time.Date(2026, 9, 1, 0, 0, 0, 0, sh),
		},
		{
			name: "重置日未到_取上月",
			now:  time.Date(2026, 9, 5, 10, 30, 0, 0, sh),
			day:  10,
			loc:  sh,
			want: time.Date(2026, 8, 10, 0, 0, 0, 0, sh),
		},
		{
			name: "跨年_一月里取上一年十二月",
			now:  time.Date(2026, 1, 5, 10, 30, 0, 0, sh),
			day:  10,
			loc:  sh,
			want: time.Date(2025, 12, 10, 0, 0, 0, 0, sh),
		},
		{
			name: "恰好落在重置时刻_取当刻而不是上月",
			now:  time.Date(2026, 9, 1, 0, 0, 0, 0, sh),
			day:  1,
			loc:  sh,
			want: time.Date(2026, 9, 1, 0, 0, 0, 0, sh),
		},
		{
			// 31 号订阅的用户，二月按当月最后一天重置。不做钳制的话
			// time.Date 会把 2 月 31 日归一成 3 月 3 日，重置时刻跑到
			// 下个月去，而这一步不会有任何报错。
			name: "31号_二月退到28号",
			now:  time.Date(2026, 3, 15, 12, 0, 0, 0, sh),
			day:  31,
			loc:  sh,
			want: time.Date(2026, 2, 28, 0, 0, 0, 0, sh),
		},
		{
			// 回退一个月不能用 AddDate：它作用在已经钳制过的 4 月 30 日上
			// 会回到 3 月 30 日，而正确答案是 3 月 31 日。
			name: "31号_四月里往前取三月31号",
			now:  time.Date(2026, 4, 1, 12, 0, 0, 0, sh),
			day:  31,
			loc:  sh,
			want: time.Date(2026, 3, 31, 0, 0, 0, 0, sh),
		},
		{
			name: "29号_平年二月退到28号",
			now:  time.Date(2026, 3, 1, 12, 0, 0, 0, sh),
			day:  29,
			loc:  sh,
			want: time.Date(2026, 2, 28, 0, 0, 0, 0, sh),
		},
		{
			name: "29号_闰年二月就是29号",
			now:  time.Date(2024, 3, 1, 12, 0, 0, 0, sh),
			day:  29,
			loc:  sh,
			want: time.Date(2024, 2, 29, 0, 0, 0, 0, sh),
		},
		{
			name: "31号_一月往前跨年取上一年十二月31号",
			now:  time.Date(2026, 1, 15, 12, 0, 0, 0, sh),
			day:  31,
			loc:  sh,
			want: time.Date(2025, 12, 31, 0, 0, 0, 0, sh),
		},
		{
			// 按面板时区对齐，不按 UTC。UTC+8 下按 UTC 切月，
			// 「本月用量」会整体错位 8 小时，且不报任何错。
			name: "按面板时区对齐_不按机器本地时区",
			now:  time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC),
			day:  1,
			loc:  ny,
			want: time.Date(2026, 8, 1, 0, 0, 0, 0, ny),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resetInstantFor(tc.now, tc.day, tc.loc)
			if got != tc.want.UnixMilli() {
				t.Errorf("resetInstantFor = %v, want %v",
					time.UnixMilli(got).In(tc.loc), tc.want)
			}
		})
	}
}

func TestResetDueTrafficZeroesOnlyInboundsPastTheirResetInstant(t *testing.T) {
	setupDB(t)
	sh := mustLoad(t, "Asia/Shanghai")
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, sh)
	thisPeriod := resetInstantFor(now, 1, sh)

	due := makeInboundFor(t, 10011, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetMonthly
		in.LastResetAt = thisPeriod - 30*testDayMillis // 上个周期清的
		in.Up, in.Down = 111, 222
	})
	fresh := makeInboundFor(t, 10012, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetMonthly
		in.LastResetAt = thisPeriod // 本周期已经清过
		in.Up, in.Down = 333, 444
	})
	off := makeInboundFor(t, 10013, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetOff
		in.LastResetAt = 0
		in.Up, in.Down = 555, 666
	})

	s := InboundService{}
	reset, reEnabled, err := s.ResetDueTraffic(now, sh)
	if err != nil {
		t.Fatalf("ResetDueTraffic: %v", err)
	}
	if reset != 1 {
		t.Errorf("reset = %d, want 1", reset)
	}
	if reEnabled != 0 {
		t.Errorf("reEnabled = %d, want 0", reEnabled)
	}

	if got := reloadInbound(t, due.Id); got.Up != 0 || got.Down != 0 {
		t.Errorf("到期的入站没有被清零: up=%d down=%d", got.Up, got.Down)
	} else if got.LastResetAt != thisPeriod {
		t.Errorf("LastResetAt = %d, want %d（本周期的重置时刻）", got.LastResetAt, thisPeriod)
	}
	if got := reloadInbound(t, fresh.Id); got.Up != 333 || got.Down != 444 {
		t.Errorf("本周期已清过的入站被重复清零: up=%d down=%d", got.Up, got.Down)
	}
	if got := reloadInbound(t, off.Id); got.Up != 555 || got.Down != 666 {
		t.Errorf("关闭重置的入站被清零: up=%d down=%d", got.Up, got.Down)
	}
}

// 重新启用是这个功能真正的价值所在，但它必须精确命中「因超流量被自动停用」
// 的那一批。管理员手动关掉的用户下个月自己活过来，是个静默的错误行为：
// 面板不会有任何提示，管理员也不会想到去复查。
func TestResetDueTrafficReEnablesOnlyTrafficDisabledInbounds(t *testing.T) {
	setupDB(t)
	sh := mustLoad(t, "Asia/Shanghai")
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, sh)
	nowMs := now.UnixMilli()
	stale := resetInstantFor(now, 1, sh) - 30*testDayMillis

	byTraffic := makeInboundFor(t, 10011, func(in *model.Inbound) {
		in.TrafficResetMode, in.LastResetAt = model.TrafficResetMonthly, stale
		in.Enable, in.DisabledByTraffic = false, true
		in.Up, in.Down, in.Total = 500, 500, 1000
	})
	byAdmin := makeInboundFor(t, 10012, func(in *model.Inbound) {
		in.TrafficResetMode, in.LastResetAt = model.TrafficResetMonthly, stale
		in.Enable, in.DisabledByTraffic = false, false // 管理员手动停用
		in.Up, in.Down = 500, 500
	})
	expired := makeInboundFor(t, 10013, func(in *model.Inbound) {
		in.TrafficResetMode, in.LastResetAt = model.TrafficResetMonthly, stale
		in.Enable, in.DisabledByTraffic = false, true
		in.ExpiryTime = nowMs - testDayMillis // 昨天就过期了
		in.Up, in.Down = 500, 500
	})

	s := InboundService{}
	reset, reEnabled, err := s.ResetDueTraffic(now, sh)
	if err != nil {
		t.Fatalf("ResetDueTraffic: %v", err)
	}
	if reset != 3 {
		t.Errorf("reset = %d, want 3（三条都该清零）", reset)
	}
	if reEnabled != 1 {
		t.Errorf("reEnabled = %d, want 1", reEnabled)
	}

	if got := reloadInbound(t, byTraffic.Id); !got.Enable {
		t.Error("因超流量被停用的入站没有被重新启用")
	} else if got.DisabledByTraffic {
		t.Error("重新启用后 DisabledByTraffic 没有被清掉")
	}
	if got := reloadInbound(t, byAdmin.Id); got.Enable {
		t.Error("管理员手动停用的入站被误启用了")
	}
	if got := reloadInbound(t, expired.Id); got.Enable {
		t.Error("已过期的入站被误启用了")
	}
}

// 「按订阅周期」取的是到期时间那一天的日号：3 月 28 日订阅、付一年，到期
// 时间是明年 3 月 28 日，于是每月 28 号重置。
func TestResetDueTrafficUsesExpiryDayForBillCycle(t *testing.T) {
	setupDB(t)
	sh := mustLoad(t, "Asia/Shanghai")
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, sh)

	in := makeInboundFor(t, 10011, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetBillCycle
		in.ExpiryTime = time.Date(2027, 3, 28, 0, 0, 0, 0, sh).UnixMilli()
		// 上一个周期点是 8/28，这里停在它之前，本轮该重置
		in.LastResetAt = time.Date(2026, 7, 28, 0, 0, 0, 0, sh).UnixMilli()
		in.Up, in.Down = 111, 222
	})

	if _, _, err := (&InboundService{}).ResetDueTraffic(now, sh); err != nil {
		t.Fatalf("ResetDueTraffic: %v", err)
	}

	got := reloadInbound(t, in.Id)
	if got.Up != 0 || got.Down != 0 {
		t.Errorf("没有按订阅日重置: up=%d down=%d", got.Up, got.Down)
	}
	want := time.Date(2026, 8, 28, 0, 0, 0, 0, sh).UnixMilli()
	if got.LastResetAt != want {
		t.Errorf("LastResetAt = %v, want %v（8 月 28 日那个周期点）",
			time.UnixMilli(got.LastResetAt).In(sh), time.UnixMilli(want).In(sh))
	}
}

// 「按订阅周期」的日号跟着到期时间走，所以到期日落在月末时，重置日也跟着
// 按当月最后一天算。31 日订阅的用户在二月不能被静默跳过一个月。
func TestResetDueTrafficClampsBillCycleToMonthEnd(t *testing.T) {
	setupDB(t)
	sh := mustLoad(t, "Asia/Shanghai")
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, sh)

	in := makeInboundFor(t, 10011, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetBillCycle
		in.ExpiryTime = time.Date(2027, 1, 31, 0, 0, 0, 0, sh).UnixMilli()
		in.LastResetAt = time.Date(2026, 1, 31, 0, 0, 0, 0, sh).UnixMilli()
		in.Up, in.Down = 111, 222
	})

	if _, _, err := (&InboundService{}).ResetDueTraffic(now, sh); err != nil {
		t.Fatalf("ResetDueTraffic: %v", err)
	}

	got := reloadInbound(t, in.Id)
	want := time.Date(2026, 2, 28, 0, 0, 0, 0, sh).UnixMilli()
	if got.LastResetAt != want {
		t.Errorf("LastResetAt = %v, want %v（二月按最后一天）",
			time.UnixMilli(got.LastResetAt).In(sh), time.UnixMilli(want).In(sh))
	}
}

// 停用原因必须分开记：过期与超流量都会把 enable 置 false，但只有后者
// 在下一个周期该被拉回来。两者同时成立时按「过期」算——过期是更强的
// 停用理由，续期之外的任何路径都不该让它自己活过来。
func TestDisableInvalidInboundsMarksOnlyTrafficExhaustion(t *testing.T) {
	setupDB(t)
	now := nowMillis()

	overQuota := makeInboundFor(t, 10011, func(in *model.Inbound) {
		in.Total, in.Up, in.Down = 1000, 600, 400
	})
	expired := makeInboundFor(t, 10012, func(in *model.Inbound) {
		in.ExpiryTime = now - testDayMillis
	})
	both := makeInboundFor(t, 10013, func(in *model.Inbound) {
		in.Total, in.Up, in.Down = 1000, 600, 400
		in.ExpiryTime = now - testDayMillis
	})
	healthy := makeInboundFor(t, 10014, func(in *model.Inbound) {
		in.Total, in.Up, in.Down = 1000, 1, 1
	})

	s := InboundService{}
	count, err := s.DisableInvalidInbounds()
	if err != nil {
		t.Fatalf("DisableInvalidInbounds: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}

	if got := reloadInbound(t, overQuota.Id); got.Enable || !got.DisabledByTraffic {
		t.Errorf("超流量停用: enable=%v disabledByTraffic=%v，want false/true",
			got.Enable, got.DisabledByTraffic)
	}
	if got := reloadInbound(t, expired.Id); got.Enable || got.DisabledByTraffic {
		t.Errorf("过期停用: enable=%v disabledByTraffic=%v，want false/false",
			got.Enable, got.DisabledByTraffic)
	}
	if got := reloadInbound(t, both.Id); got.Enable || got.DisabledByTraffic {
		t.Errorf("同时过期与超流量: enable=%v disabledByTraffic=%v，want false/false（按过期算）",
			got.Enable, got.DisabledByTraffic)
	}
	if got := reloadInbound(t, healthy.Id); !got.Enable || got.DisabledByTraffic {
		t.Errorf("正常入站被动了: enable=%v disabledByTraffic=%v",
			got.Enable, got.DisabledByTraffic)
	}
}

// 模式只有三个合法值。脏数据（比如将来加了第四种模式又回退了版本）不能
// 被当成「某种重置」去执行——宁可不重置让管理员察觉。
func TestInboundRejectsUnknownTrafficResetMode(t *testing.T) {
	for _, mode := range []int{-1, 3, 99} {
		t.Run(fmt.Sprintf("mode=%d", mode), func(t *testing.T) {
			setupDB(t)
			s := InboundService{}

			add := &model.Inbound{
				UserId: 1, Port: 10011, Protocol: model.VLESS, Remark: "甲", Enable: true,
				Settings:         `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811"}],"decryption":"none"}`,
				StreamSettings:   `{"network":"tcp"}`,
				Sniffing:         `{"enabled":true}`,
				TrafficResetMode: mode,
			}
			if err := s.AddInbound(add); err == nil {
				t.Error("AddInbound 接受了未知的重置模式")
			}

			exist := makeInboundFor(t, 10012, func(in *model.Inbound) {})
			edit := *exist
			edit.TrafficResetMode = mode
			if err := s.UpdateInbound(&edit); err == nil {
				t.Error("UpdateInbound 接受了未知的重置模式")
			}
		})
	}
}

// 「按订阅周期」的日号来自到期时间。没有到期时间就没有订阅周期，这个组合
// 本身没有定义——必须在保存时就拒绝，而不是存下来静默地永不生效。
// 前端会在到期时间为空时禁掉这个选项，但那不是防线：请求可以直接构造。
func TestInboundRejectsBillCycleWithoutExpiryTime(t *testing.T) {
	setupDB(t)
	s := InboundService{}

	add := &model.Inbound{
		UserId: 1, Port: 10011, Protocol: model.VLESS, Remark: "甲", Enable: true,
		Settings:         `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811"}],"decryption":"none"}`,
		StreamSettings:   `{"network":"tcp"}`,
		Sniffing:         `{"enabled":true}`,
		TrafficResetMode: model.TrafficResetBillCycle,
		ExpiryTime:       0,
	}
	if err := s.AddInbound(add); err == nil {
		t.Error("AddInbound 接受了「按订阅周期」+ 无到期时间")
	}

	exist := makeInboundFor(t, 10012, func(in *model.Inbound) {})
	edit := *exist
	edit.TrafficResetMode = model.TrafficResetBillCycle
	edit.ExpiryTime = 0
	if err := s.UpdateInbound(&edit); err == nil {
		t.Error("UpdateInbound 接受了「按订阅周期」+ 无到期时间")
	}
}

// 管理员碰过 enable，自动停用的理由就不再成立。不清标记的话，一个被手动
// 停用的入站会在下一个重置周期自己活过来，而面板不会有任何提示。
func TestUpdateInboundClearsDisabledByTraffic(t *testing.T) {
	setupDB(t)
	in := makeInboundFor(t, 10011, func(in *model.Inbound) {
		in.Enable, in.DisabledByTraffic = false, true
		in.Total, in.Up, in.Down = 1000, 600, 400
	})

	s := InboundService{}
	edit := *in
	edit.Remark = "改个备注"
	if err := s.UpdateInbound(&edit); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}

	if got := reloadInbound(t, in.Id); got.DisabledByTraffic {
		t.Error("UpdateInbound 之后 DisabledByTraffic 仍为 true")
	}
}

// 切换重置模式必须把 LastResetAt 顶到当前时刻。否则改模式会立刻触发一次
// 计划外的清零：从「按订阅周期(28 号)」改到「每月 1 号」时，上次重置停在
// 8/28，而 9/1 这个新周期点已经过去，任务下一轮就会立刻清一次。
func TestUpdateInboundStampsLastResetAtWhenModeChanges(t *testing.T) {
	setupDB(t)
	in := makeInboundFor(t, 10011, func(in *model.Inbound) {
		in.TrafficResetMode, in.LastResetAt = model.TrafficResetOff, 0
		in.Up, in.Down = 111, 222
	})

	s := InboundService{}
	before := nowMillis()
	turnOn := *in
	turnOn.TrafficResetMode = model.TrafficResetMonthly
	if err := s.UpdateInbound(&turnOn); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}

	stamped := reloadInbound(t, in.Id)
	if stamped.TrafficResetMode != model.TrafficResetMonthly {
		t.Fatalf("TrafficResetMode = %d, want %d",
			stamped.TrafficResetMode, model.TrafficResetMonthly)
	}
	if stamped.LastResetAt < before {
		t.Fatalf("LastResetAt = %d，want >= %d（切换那一刻）", stamped.LastResetAt, before)
	}

	// 再改一次、模式不变：这不是「切换」，不该重写时刻，否则每编辑一次
	// 入站就把重置周期往后推一次，重置永远轮不到。
	again := *stamped
	again.Remark = "再改个备注"
	if err := s.UpdateInbound(&again); err != nil {
		t.Fatalf("UpdateInbound 二次: %v", err)
	}
	if got := reloadInbound(t, in.Id); got.LastResetAt != stamped.LastResetAt {
		t.Errorf("模式没变却重写了 LastResetAt: %d -> %d", stamped.LastResetAt, got.LastResetAt)
	}
}

// 续期与编辑同理：管理员手工碰过这条记录，自动停用的理由就不再成立。
// 续到过去（等价于立即停用）同样要清——否则那条规则要靠「已过期」这一层
// 兜底，多一层耦合，而两条规则本来说的是同一件事。
func TestRenewInboundClearsDisabledByTraffic(t *testing.T) {
	cases := []struct {
		name       string
		expiryTime int64
	}{
		{name: "续到未来", expiryTime: nowMillis() + 30*testDayMillis},
		{name: "续到过去_等价于立即停用", expiryTime: nowMillis() - testDayMillis},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupDB(t)
			in := makeInboundFor(t, 10011, func(in *model.Inbound) {
				in.Enable, in.DisabledByTraffic = false, true
				in.Total, in.Up, in.Down = 1000, 600, 400
			})

			s := InboundService{}
			if err := s.RenewInbound(in.Id, 0, tc.expiryTime); err != nil {
				t.Fatalf("RenewInbound: %v", err)
			}
			if got := reloadInbound(t, in.Id); got.DisabledByTraffic {
				t.Error("续期之后 DisabledByTraffic 仍为 true")
			}
		})
	}
}

// 写入路径已经挡住这两种非法组合，但直接改库、或将来某条新的写入路径漏掉
// 校验，仍可能落进库里。执行期宁可不重置让管理员察觉，也不能猜一个日号
// 去清零——那是静默地清错时间。
func TestResetDueTrafficSkipsInboundsWithNoUsableResetDay(t *testing.T) {
	setupDB(t)
	sh := mustLoad(t, "Asia/Shanghai")
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, sh)

	dirty := makeInboundFor(t, 10011, func(in *model.Inbound) {
		in.TrafficResetMode = 99 // 绕开写入校验直接落库
		in.LastResetAt = 0
		in.Up, in.Down = 111, 222
	})
	noExpiry := makeInboundFor(t, 10012, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetBillCycle
		in.ExpiryTime = 0 // 同样绕开写入校验
		in.LastResetAt = 0
		in.Up, in.Down = 333, 444
	})

	s := InboundService{}
	reset, _, err := s.ResetDueTraffic(now, sh)
	if err != nil {
		t.Fatalf("ResetDueTraffic: %v", err)
	}
	if reset != 0 {
		t.Errorf("reset = %d, want 0", reset)
	}
	if got := reloadInbound(t, dirty.Id); got.Up != 111 || got.Down != 222 {
		t.Errorf("未知模式的入站被清零了: up=%d down=%d", got.Up, got.Down)
	}
	if got := reloadInbound(t, noExpiry.Id); got.Up != 333 || got.Down != 444 {
		t.Errorf("按订阅周期但没有到期时间的入站被清零了: up=%d down=%d", got.Up, got.Down)
	}
}
