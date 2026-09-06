package service

import (
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// setupMeterPoolTest 建主库与用量库。计量池落在用量库，与 DomainStat 同库。
func setupMeterPoolTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "main.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := database.InitTrafficDB(filepath.Join(dir, "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
	// 用量库句柄是包级变量，会跨用例残留——而且 SQLite 在文件被 t.TempDir
	// 清掉之后仍能通过已打开的 fd 读到旧数据。计量池就在这个库里，不清空的话
	// 本用例写进池的行会漏进 routing_inject_test.go 的用例，让 Inject 生成出
	// 无从解释的计量出站，把既有断言打成随机失败。
	t.Cleanup(database.ResetTrafficDBForTest)
}

// putPoolRow 直接写一行池记录，绕过重算逻辑，专测读取与清理。
func putPoolRow(t *testing.T, inboundId int, dom string, cooldownUntil int64) {
	t.Helper()
	row := &model.MeterDomain{
		InboundId: inboundId, Domain: dom,
		EnteredAt: time.Now().Unix(), CooldownUntil: cooldownUntil,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入池行: %v", err)
	}
}

func TestPoolIsSortedAndSkipsCooldown(t *testing.T) {
	setupMeterPoolTest(t)
	now := time.Unix(1_800_000_000, 0)
	// 故意乱序写入：Pool 必须自己排序，生成期靠它保证配置逐字节确定。
	putPoolRow(t, 7, "zeta.com", 0)
	putPoolRow(t, 3, "beta.com", 0)
	putPoolRow(t, 3, "alpha.com", 0)
	// 冷却中的行不参与生成：它已经退池，只是留着记冷却时刻。
	putPoolRow(t, 3, "cooling.com", now.Add(time.Hour).Unix())

	got, err := (&MeterPoolService{}).Pool(now)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	want := []MeterEntry{
		{InboundId: 3, Domain: "alpha.com"},
		{InboundId: 3, Domain: "beta.com"},
		{InboundId: 7, Domain: "zeta.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("Pool 返回 %d 行，期望 %d：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 行 = %+v，期望 %+v", i, got[i], want[i])
		}
	}
}

func TestPoolReturnsEmptyWhenTrafficDBMissing(t *testing.T) {
	setupMeterPoolTest(t)
	putPoolRow(t, 3, "alpha.com", 0)
	database.ResetTrafficDBForTest()
	// 用量库打不开时整个计量功能自动停用，绝不让配置生成失败。
	got, err := (&MeterPoolService{}).Pool(time.Now())
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Pool = %+v，期望空", got)
	}
}

func TestDeleteByInboundRemovesOnlyThatInbound(t *testing.T) {
	setupMeterPoolTest(t)
	putPoolRow(t, 3, "alpha.com", 0)
	putPoolRow(t, 7, "zeta.com", 0)
	if err := (&MeterPoolService{}).DeleteByInbound(3); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}
	got, err := (&MeterPoolService{}).Pool(time.Now())
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(got) != 1 || got[0].InboundId != 7 {
		t.Errorf("Pool = %+v，期望只剩入站 7 的那行", got)
	}
}

func TestPruneOrphansDropsRowsOfDeletedInbounds(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31501, "甲")
	putPoolRow(t, in.Id, "alpha.com", 0)
	putPoolRow(t, in.Id+999, "orphan.com", 0) // 这个入站不存在

	// SQLite 会复用被删除的自增 id：残留行会绑到下一个建出来的入站上，
	// 于是面板会为一个全新用户生成一批别人的域名的计量出站与规则，
	// 而引用不再悬空，生成期没有任何一道防线拦得住。
	pruned, err := (&MeterPoolService{}).PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("清理了 %d 行，期望 1", pruned)
	}
	got, _ := (&MeterPoolService{}).Pool(time.Now())
	if len(got) != 1 || got[0].InboundId != in.Id {
		t.Errorf("Pool = %+v，期望只剩存在的那个入站", got)
	}
}

func TestDelInboundClearsMeterPool(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31502, "甲")
	putPoolRow(t, in.Id, "alpha.com", 0)

	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}
	got, err := (&MeterPoolService{}).Pool(time.Now())
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Pool = %+v，期望空——删除入站必须连带清掉它的池行", got)
	}
}

// putDomainStat 直接写一行小时桶，供池的重算读取。
func putDomainStat(t *testing.T, inboundId int, dom string, bucketStart, count, up, down int64) {
	t.Helper()
	row := &model.DomainStat{
		Granularity: model.GranularityHour, InboundId: inboundId,
		Domain: dom, BucketStart: bucketStart, Count: count, Up: up, Down: down,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入域名统计: %v", err)
	}
}

// poolRows 返回某入站的全部池行（含冷却中的），按域名排序。
func poolRows(t *testing.T, inboundId int) []model.MeterDomain {
	t.Helper()
	var rows []model.MeterDomain
	if err := database.GetTrafficDB().Where("inbound_id = ?", inboundId).
		Order("domain asc").Find(&rows).Error; err != nil {
		t.Fatalf("查询池行: %v", err)
	}
	return rows
}

func TestRecomputeColdStartRanksByCount(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(0)
	in := mkTrafficInbound(t, 31601, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	bucket := now.Add(-time.Hour).Unix()
	// 一个字节都还没有：排序退化成按访问次数。
	putDomainStat(t, in.Id, "a.com", bucket, 5, 0, 0)
	putDomainStat(t, in.Id, "b.com", bucket, 50, 0, 0)

	if _, err := (&MeterPoolService{}).Recompute(now); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	rows := poolRows(t, in.Id)
	if len(rows) != 2 {
		t.Fatalf("池里 %d 行，期望 2：%+v", len(rows), rows)
	}
	// K = min(60, 200/1) = 60，两个都进得去；这里断言的是它们确实都进了池，
	// 且 EnteredAt 被写上（最小驻留闸门要用）。
	for _, r := range rows {
		if r.EnteredAt != now.Unix() {
			t.Errorf("%s 的 EnteredAt = %d，期望 %d", r.Domain, r.EnteredAt, now.Unix())
		}
	}
}

func TestRecomputeIsIdempotent(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(0)
	in := mkTrafficInbound(t, 31602, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	putDomainStat(t, in.Id, "a.com", now.Add(-time.Hour).Unix(), 5, 1000, 2000)

	svc := &MeterPoolService{}
	if _, err := svc.Recompute(now); err != nil {
		t.Fatalf("首轮 Recompute: %v", err)
	}
	before := poolRows(t, in.Id)
	changed, err := svc.Recompute(now)
	if err != nil {
		t.Fatalf("次轮 Recompute: %v", err)
	}
	if changed != 0 {
		t.Errorf("次轮变动 %d 项，期望 0——数据没变就不该换池，"+
			"每次换池都会在核心里留下一对永不回收的计数器", changed)
	}
	after := poolRows(t, in.Id)
	if len(before) != len(after) || before[0].Domain != after[0].Domain ||
		before[0].EnteredAt != after[0].EnteredAt {
		t.Errorf("池发生了变化：%+v -> %+v", before, after)
	}
}

func TestRecomputeRetiresZeroByteDomainAndCoolsItDown(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(0)
	in := mkTrafficInbound(t, 31603, "甲")
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	// 只有连接数、没有字节：典型的「已经被管理员规则分流走」或「探测器」。
	putDomainStat(t, in.Id, "zero.com", base.Add(-time.Hour).Unix(), 100, 0, 0)

	svc := &MeterPoolService{}
	// 第 0 轮是「进池」，那一轮不产生零轮计数；零轮计数从其后每一轮开始 +1，
	// 所以跑满 meterProbeGiveUpRounds(3) 需要 base+1h / +2h / +3h 三轮，
	// 退场发生在 base+3h —— 一共 4 轮。
	rounds := meterProbeGiveUpRounds + 1
	for i := 0; i < rounds; i++ {
		if _, err := svc.Recompute(base.Add(time.Duration(i) * time.Hour)); err != nil {
			t.Fatalf("第 %d 轮 Recompute: %v", i, err)
		}
	}
	rows := poolRows(t, in.Id)
	if len(rows) != 1 {
		t.Fatalf("池行 %d 条，期望 1（退场后仍保留行以记住冷却时刻）：%+v", len(rows), rows)
	}
	wantCooldown := base.Add(time.Duration(rounds-1) * time.Hour).Add(meterCooldown).Unix()
	if rows[0].CooldownUntil != wantCooldown {
		t.Errorf("CooldownUntil = %d，期望 %d", rows[0].CooldownUntil, wantCooldown)
	}
	// 冷却中的行不参与生成。
	entries, err := svc.Pool(base.Add(time.Duration(rounds) * time.Hour))
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Pool = %+v，期望空——退场的域名不该继续生成出站与规则", entries)
	}
}

func TestRecomputeFreezesWhenStaleCountersExceedLimit(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31604, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	putDomainStat(t, in.Id, "a.com", now.Add(-time.Hour).Unix(), 5, 1000, 0)

	SetStaleMeterCounters(meterStaleCounterLimit + 1)
	t.Cleanup(func() { SetStaleMeterCounters(0) })

	changed, err := (&MeterPoolService{}).Recompute(now)
	if err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if changed != 0 {
		t.Errorf("变动 %d 项，期望 0——死计数器过多时必须冻结换池", changed)
	}
	if rows := poolRows(t, in.Id); len(rows) != 0 {
		t.Errorf("池行 = %+v，期望空", rows)
	}
}

func TestRecomputeIgnoresDisabledInbounds(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(0)
	in := mkTrafficInbound(t, 31605, "甲")
	in.Enable = false
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("停用入站: %v", err)
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	putDomainStat(t, in.Id, "a.com", now.Add(-time.Hour).Unix(), 5, 1000, 0)

	if _, err := (&MeterPoolService{}).Recompute(now); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if rows := poolRows(t, in.Id); len(rows) != 0 {
		t.Errorf("池行 = %+v，期望空——停用的入站不生成计量出站，也不该占预算", rows)
	}
}

func TestPoolExcludesRowsWhoseCooldownAlreadyExpired(t *testing.T) {
	setupMeterPoolTest(t)
	now := time.Unix(1_800_000_000, 0)
	putPoolRow(t, 3, "alive.com", 0)
	// 已退场、冷却已过期、还没被下一次 Recompute 删掉。它绝不能被当成在池：
	// 判据若是 cooldown_until <= now，这一行就会回到生成配置里，试用退场闸门
	// 每 24 小时被规律性击穿一次（清理只在 Recompute 顶部跑，而面板重启会打乱
	// cron 相位，窗口均匀分布在 [0, 1h)）。冻结分支更甚：它在清理之前就 return，
	// 这些行会一直被当成在池，把 RecordMetered 的死计数器观测值压低，冻结提前
	// 解除，方向与冻结的目的正好相反。
	putPoolRow(t, 3, "expired.com", now.Add(-time.Hour).Unix())
	// 冷却中的行本来就不该返回，一并守住。
	putPoolRow(t, 3, "cooling.com", now.Add(time.Hour).Unix())

	got, err := (&MeterPoolService{}).Pool(now)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	want := []MeterEntry{{InboundId: 3, Domain: "alive.com"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("Pool = %+v，期望 %+v——只有 cooldown_until = 0 的行才在池内", got, want)
	}
}
