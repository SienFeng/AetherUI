package service

import (
	"fmt"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// seedIPHour 往库里塞一行，供本文件各用例复用。
func seedIPHour(t *testing.T, inboundId int, ip string, hourStart int64, up, down int64) {
	t.Helper()
	row := model.InboundIPHour{
		InboundId: inboundId, IP: ip, HourStart: hourStart,
		ActiveSeconds: 120, ActiveBytes: up + down,
		ActiveUp: up, ActiveDown: down,
	}
	if err := database.GetTrafficDB().Create(&row).Error; err != nil {
		t.Fatalf("写入 InboundIPHour: %v", err)
	}
}

// 基本聚合：同一个 IP 的多个小时求和，按用量降序排列。
func TestIPUsageAggregatesAndSortsByUsage(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	seedIPHour(t, 1, "1.1.1.1", 3600, 100, 200)
	seedIPHour(t, 1, "1.1.1.1", 7200, 300, 400)
	seedIPHour(t, 1, "2.2.2.2", 3600, 5000, 6000)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("条数 = %d，期望 2", len(got.Entries))
	}
	// 用量大的排前面。
	if got.Entries[0].IP != "2.2.2.2" {
		t.Errorf("第一条 = %s，期望用量最大的 2.2.2.2", got.Entries[0].IP)
	}
	if got.Entries[1].Up != 400 || got.Entries[1].Down != 600 {
		t.Errorf("1.1.1.1 的 Up/Down = %d/%d，期望 400/600（两个小时求和）",
			got.Entries[1].Up, got.Entries[1].Down)
	}
	if got.Entries[1].LastSeen != 7200*1000 {
		t.Errorf("LastSeen = %d，期望 %d 毫秒（窗口内最后一个有记录的小时）",
			got.Entries[1].LastSeen, 7200*1000)
	}
}

// 窗口是左闭右开：恰好落在 End 上的桶不算进来。
//
// 不这么做的话，相邻两个窗口会把边界那个小时各算一次，「今日」与「昨日」
// 相加会大于「近 2 日」，而没有任何一层会报错。
func TestIPUsageWindowIsHalfOpen(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 3600, End: 7200, Granularity: model.GranularityHour}

	seedIPHour(t, 1, "1.1.1.1", 3600, 10, 20) // 在窗口内
	seedIPHour(t, 1, "1.1.1.1", 7200, 99, 99) // 恰好在右端，不算

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Up != 10 {
		t.Fatalf("Up = %v，期望只统计窗口内那一行（10）", got.Entries)
	}
}

// 只统计指定入站的行。
//
// SQLite 会复用被删除的自增 id，漏掉 inbound_id 条件会让一个入站看到
// 别人的来源 IP，而表格渲染得完全合理。
func TestIPUsageIsScopedToInbound(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	seedIPHour(t, 1, "1.1.1.1", 3600, 10, 20)
	seedIPHour(t, 2, "9.9.9.9", 3600, 10, 20)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].IP != "1.1.1.1" {
		t.Fatalf("得到 %v，期望只有入站 1 的来源", got.Entries)
	}
}

// 升级前的老行（ActiveBytes 有值、两个新列为 0）整批降级，Split 为 false。
//
// 判据按整批而非逐行，与 hasActiveBytes 那条完全同构：逐行判会有歧义
// ——一条恰好在本小时没有字节增量的新行，与升级前写入的老行长得一模一样。
func TestIPUsageSplitIsBatchWideNotPerRow(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	// 全是老行。
	for _, ip := range []string{"1.1.1.1", "2.2.2.2"} {
		row := model.InboundIPHour{
			InboundId: 1, IP: ip, HourStart: 3600,
			ActiveSeconds: 120, ActiveBytes: 1000, // ActiveUp/ActiveDown 为零值
		}
		if err := database.GetTrafficDB().Create(&row).Error; err != nil {
			t.Fatalf("写入老行: %v", err)
		}
	}

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Split {
		t.Error("Split = true，期望 false（整批都是升级前的老行）")
	}
	// 降级时仍要给出合计，不能因为拆不出上下行就返回 0。
	if got.Entries[0].Up+got.Entries[0].Down != 1000 {
		t.Errorf("合计 = %d，期望 1000（降级只影响拆分，不影响总量）",
			got.Entries[0].Up+got.Entries[0].Down)
	}
}

// 混合批次（窗口里同时有升级前的老行与升级后的新行）必须整批按合计口径，
// 且**一个字节都不能丢**。
//
// 这条用例是为一个真实缺陷写的：判据曾经是 any 语义（任一行有拆分即
// Split=true），于是混合批次里老行走了拆分分支，而它的 ActiveUp/ActiveDown
// 恒为 0，ActiveBytes 从未被计入——流量凭空消失，没有任何一层会报错。
// 所以这里必须同时断言「降级标志」和「总量」，只断言前者是测不出那个缺陷的。
func TestIPUsageMixedBatchDegradesAndKeepsTotal(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	// 升级前的老行：ActiveBytes 有值，两个新列是零值。
	old := model.InboundIPHour{
		InboundId: 1, IP: "1.1.1.1", HourStart: 3600,
		ActiveSeconds: 120, ActiveBytes: 1000,
	}
	if err := database.GetTrafficDB().Create(&old).Error; err != nil {
		t.Fatalf("写入老行: %v", err)
	}
	// 升级后的新行。
	seedIPHour(t, 1, "2.2.2.2", 7200, 300, 700)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Split {
		t.Error("Split = true，期望 false（批里有一行拆不出，整批应按合计口径）")
	}

	var total int64
	for _, e := range got.Entries {
		total += e.Up + e.Down
	}
	if total != 2000 {
		t.Errorf("总量 = %d，期望 2000（老行 1000 + 新行 300+700）——"+
			"老行的字节不能因为拆不出上下行就凭空消失", total)
	}
}

// 窗口超出 InboundIPHour 的保留期时整块降级：BeyondRetention 为 true、
// Entries 为空、Reason 非空。
//
// 返回一张看起来正常的空表会让管理员以为这段时间没人用过。
func TestIPUsageBeyondRetentionDegradesWithReason(t *testing.T) {
	setupSharingTest(t)
	now := time.Now()
	w := TrafficWindow{
		Start:       now.AddDate(0, 0, -200).Unix(),
		End:         now.Unix(),
		Granularity: model.GranularityDay,
	}
	seedIPHour(t, 1, "1.1.1.1", now.Add(-time.Hour).Unix(), 10, 20)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !got.BeyondRetention {
		t.Error("BeyondRetention = false，期望 true")
	}
	if len(got.Entries) != 0 {
		t.Errorf("Entries 有 %d 条，期望空", len(got.Entries))
	}
	if got.Reason == "" {
		t.Error("Reason 为空——「看不到」必须和「没有」能区分开")
	}
}

// 长尾截断：超过上限时只返回前 N 条，其余合并成 Other，总量不缩水。
func TestIPUsageTruncatesLongTailWithoutLosingTotal(t *testing.T) {
	setupSharingTest(t)
	// 窗口跨度必须 <= ipUsageMaxWindowDays(30)，否则 Query 会走 BeyondRetention
	// 早退、Entries 为空，这条用例就根本测不到截断逻辑。1000~100000 约 1.15 天。
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	var wantUp, wantDown int64
	total := ipUsageMaxEntries + 30
	for i := 0; i < total; i++ {
		up := int64(total - i) // 递减，保证排序稳定可断言
		down := up * 2
		seedIPHour(t, 1, fmt.Sprintf("10.0.%d.%d", i/256, i%256), 3600, up, down)
		wantUp += up
		wantDown += down
	}

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Entries) != ipUsageMaxEntries {
		t.Fatalf("条数 = %d，期望 %d", len(got.Entries), ipUsageMaxEntries)
	}
	if got.OtherCount != 30 {
		t.Errorf("OtherCount = %d，期望 30", got.OtherCount)
	}

	var gotUp, gotDown int64
	for _, e := range got.Entries {
		gotUp += e.Up
		gotDown += e.Down
	}
	gotUp += got.OtherUp
	gotDown += got.OtherDown
	if gotUp != wantUp || gotDown != wantDown {
		t.Errorf("截断后总量 %d/%d，期望 %d/%d——截断绝不能让总量缩水",
			gotUp, gotDown, wantUp, wantDown)
	}
}

// 用量相同时按 IP 字节序排，保证同一份输入永远给出同一个次序。
func TestIPUsageOrderIsDeterministicOnTiedUsage(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	seedIPHour(t, 1, "2.2.2.2", 3600, 100, 100)
	seedIPHour(t, 1, "1.1.1.1", 3600, 100, 100)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Entries[0].IP != "1.1.1.1" {
		t.Errorf("同量时第一条 = %s，期望字节序在前的 1.1.1.1", got.Entries[0].IP)
	}
}

// 一条「有活跃时长但本小时零字节」的新行不得把整批拖进降级。
//
// 它与升级前的老行在数值上完全无法区分（ActiveUp/ActiveDown 都是 0），
// 但它的 ActiveBytes 也是 0——在两种口径下都贡献 0，不影响求和，也就
// 不该影响口径判定。
func TestIPUsageZeroByteRowDoesNotForceDegrade(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	zero := model.InboundIPHour{
		InboundId: 1, IP: "1.1.1.1", HourStart: 3600,
		ActiveSeconds: 120, ActiveBytes: 0,
	}
	if err := database.GetTrafficDB().Create(&zero).Error; err != nil {
		t.Fatalf("写入零字节行: %v", err)
	}
	seedIPHour(t, 1, "2.2.2.2", 7200, 300, 700)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !got.Split {
		t.Error("Split = false，期望 true（零字节行不该把纯新数据的批次拖进降级）")
	}
}
