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
