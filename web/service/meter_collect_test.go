package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

func TestRecordMeteredWritesBothGranularities(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31701, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	// 面板时区默认 Asia/Shanghai：UTC 17:30 是当地次日 01:30，
	// 小时桶落在当地 01:00，日桶落在当地 00:00。
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	traffics := []*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 999, Down: 999},
		{IsInbound: false, Tag: model.MeterTag(in.Id, "doubleclick.net"), Up: 100, Down: 200},
	}
	if err := (&DomainStatService{}).RecordMetered(traffics, now); err != nil {
		t.Fatalf("RecordMetered: %v", err)
	}

	for _, g := range []model.TrafficGranularity{model.GranularityHour, model.GranularityDay} {
		rows := listDomainStats(t, g)
		if len(rows) != 1 {
			t.Fatalf("粒度 %d 有 %d 行，期望 1：%+v", g, len(rows), rows)
		}
		if rows[0].Domain != "doubleclick.net" || rows[0].Up != 100 || rows[0].Down != 200 {
			t.Errorf("粒度 %d 的行 = %+v，期望 doubleclick.net 上传 100 下载 200", g, rows[0])
		}
		// 字节来自计量出站，次数来自访问日志聚合；这一轮没有访问日志，
		// 所以 Count 必须是 0——两个数据源写同一行是设计如此。
		if rows[0].Count != 0 {
			t.Errorf("粒度 %d 的 Count = %d，期望 0", g, rows[0].Count)
		}
	}
}

func TestRecordMeteredAccumulates(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31702, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	tag := model.MeterTag(in.Id, "doubleclick.net")
	svc := &DomainStatService{}

	for i := 0; i < 3; i++ {
		// 同一个桶在一小时内会被 360 轮采集写到，必须累加而不是覆盖。
		if err := svc.RecordMetered([]*xray.Traffic{{Tag: tag, Up: 10, Down: 20}}, now); err != nil {
			t.Fatalf("第 %d 轮: %v", i, err)
		}
	}
	rows := listDomainStats(t, model.GranularityHour)
	if len(rows) != 1 || rows[0].Up != 30 || rows[0].Down != 60 {
		t.Errorf("累加结果 = %+v，期望上传 30 下载 60", rows)
	}
}

func TestRecordMeteredSkipsZeroAndUnparsable(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31703, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)

	err := (&DomainStatService{}).RecordMetered([]*xray.Traffic{
		// 零增量不写行：xray 对每个已注册的计数器都会返回一行（包括值为 0
		// 的），退过池的 tag 更是会一直返回 0。写进去只会造出一堆空桶。
		{Tag: model.MeterTag(in.Id, "doubleclick.net"), Up: 0, Down: 0},
		// 形态不对的 tag 一律跳过：硬猜只会把字节记到错的域名上。
		{Tag: "a-ui-meter-x-bad.com", Up: 100, Down: 100},
		{Tag: "a-ui-meter-", Up: 100, Down: 100},
		// 入站条目与普通出站条目都不属于这里。
		{IsInbound: true, Tag: in.Tag, Up: 100, Down: 100},
		{Tag: "a-ui-hk", Up: 100, Down: 100},
	}, now)
	if err != nil {
		t.Fatalf("RecordMetered: %v", err)
	}
	if rows := listDomainStats(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("写了 %d 行，期望 0：%+v", len(rows), rows)
	}
}

func TestRecordMeteredObservesStaleCounters(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31704, "甲")
	putPoolRow(t, in.Id, "in-pool.com", 0)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	SetStaleMeterCounters(0)
	t.Cleanup(func() { SetStaleMeterCounters(0) })

	// xray 的 RemoveHandler 不注销 stats 计数器，也没有注销 RPC，所以退过池
	// 的 tag 会一直被 QueryStats 返回（值为 0）。数出来供 Recompute 决定
	// 要不要冻结换池。
	err := (&DomainStatService{}).RecordMetered([]*xray.Traffic{
		{Tag: model.MeterTag(in.Id, "in-pool.com"), Up: 1, Down: 1},
		{Tag: model.MeterTag(in.Id, "gone-1.com")},
		{Tag: model.MeterTag(in.Id, "gone-2.com")},
	}, now)
	if err != nil {
		t.Fatalf("RecordMetered: %v", err)
	}
	if got := StaleMeterCounters(); got != 2 {
		t.Errorf("观测到 %d 个死计数器，期望 2", got)
	}
}

func TestRecordMeteredNoopWithoutMeterEntries(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(7)
	t.Cleanup(func() { SetStaleMeterCounters(0) })
	// 完全没有计量条目时也要把观测值归零：xray 重启之后计数器全清，
	// 观测值必须跟着归零，冻结才能自动解除。
	err := (&DomainStatService{}).RecordMetered([]*xray.Traffic{
		{IsInbound: true, Tag: "inbound-1", Up: 1, Down: 1},
	}, time.Now())
	if err != nil {
		t.Fatalf("RecordMetered: %v", err)
	}
	if got := StaleMeterCounters(); got != 0 {
		t.Errorf("观测值 = %d，期望 0", got)
	}
}

func TestRecordMeteredSilentWhenTrafficDBMissing(t *testing.T) {
	setupMeterPoolTest(t)
	database.ResetTrafficDBForTest()
	// 库不可用时静默返回：字节数据少一段，比让 AddTraffic 整个失败轻得多。
	err := (&DomainStatService{}).RecordMetered(
		[]*xray.Traffic{{Tag: "a-ui-meter-1-x.com", Up: 1, Down: 1}}, time.Now())
	if err != nil {
		t.Errorf("RecordMetered = %v，期望 nil", err)
	}
}
