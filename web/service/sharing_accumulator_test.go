package service

import (
	"fmt"
	"testing"
	"time"

	"a-ui/database/model"
)

func oneObs() []sharingObservation {
	return []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Province: "江苏"}}
}

// 入站端口在公网上会被扫。若每个建立过连接的来源都落一行，一次端口扫描
// 就能在一小时内塞进几千行。60 秒门槛把一次性探测挡在外面。
func TestAccumulatorRequiresThresholdBeforeFlush(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	if got := a.observe(base, oneObs(), 30); len(got) != 0 {
		t.Fatalf("累计 30 秒就落库了: %+v", got)
	}
	got := a.observe(base.Add(30*time.Second), oneObs(), 30)
	if len(got) != 1 || got[0].ActiveSeconds != 60 {
		t.Fatalf("累计 60 秒 = %+v, want 1 条 ActiveSeconds=60", got)
	}
	if got := a.observe(base.Add(60*time.Second), oneObs(), 30); len(got) != 0 {
		t.Fatalf("距上次落库只多 30 秒，不该再落库: %+v", got)
	}
	got = a.observe(base.Add(90*time.Second), oneObs(), 30)
	if len(got) != 1 || got[0].ActiveSeconds != 120 {
		t.Fatalf("累计 120 秒 = %+v, want 1 条 ActiveSeconds=120", got)
	}
}

// 不满门槛的余量跨小时直接丢弃，不结转——结转会让一个每小时只用 30 秒的
// 扫描器攒几轮就绕开门槛。
func TestAccumulatorDiscardsSubThresholdRemainderAcrossHours(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 59, 30, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	if got := a.observe(h10, oneObs(), 30); len(got) != 0 {
		t.Fatalf("不该落库: %+v", got)
	}
	if got := a.observe(h11, oneObs(), 30); len(got) != 0 {
		t.Fatalf("跨小时结转了不满门槛的余量: %+v", got)
	}
}

// 已过门槛但最后一段还没落库的，跨小时时要补一次收尾写入，否则那段时长
// 永远丢失。
func TestAccumulatorFlushesCompletedHourOnRollover(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	a.observe(h10, oneObs(), 30)                        // 30
	a.observe(h10.Add(30*time.Second), oneObs(), 30)    // 60，落库
	a.observe(h10.Add(60*time.Second), oneObs(), 30)    // 90，未落库

	got := a.observe(h11, oneObs(), 30)
	if len(got) != 1 {
		t.Fatalf("跨小时 = %+v, want 1 条收尾记录", got)
	}
	if got[0].HourStart != model.AlignHourUTC(h10) {
		t.Errorf("收尾记录落在 %v, want 上一小时 %v", got[0].HourStart, model.AlignHourUTC(h10))
	}
	if got[0].ActiveSeconds != 90 {
		t.Errorf("收尾记录 ActiveSeconds = %v, want 90", got[0].ActiveSeconds)
	}
}

// 上限不是为正常场景设的（正常一小时 2~3 行），是为被针对性刷时让表的
// 大小有确定天花板。
func TestAccumulatorCapsRowsPerInboundPerHour(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	obs := make([]sharingObservation, 0, sharingMaxRowsPerHour+10)
	for i := 0; i < sharingMaxRowsPerHour+10; i++ {
		obs = append(obs, sharingObservation{
			InboundId: 1,
			IP:        fmt.Sprintf("10.0.%v.%v", i/256, i%256),
			Province:  "江苏",
		})
	}
	a.observe(base, obs, 30)
	got := a.observe(base.Add(30*time.Second), obs, 30)
	if len(got) != sharingMaxRowsPerHour {
		t.Errorf("落库 %v 条, want 上限 %v 条", len(got), sharingMaxRowsPerHour)
	}
}

// 上限只挡新来源，已在累计的继续累计——否则一次扫描就能把真实用户从
// 表里挤掉。
func TestAccumulatorCapDoesNotEvictExistingSources(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	real := sharingObservation{InboundId: 1, IP: "1.1.1.1", Province: "江苏"}

	a.observe(base, []sharingObservation{real}, 30)

	flood := []sharingObservation{real}
	for i := 0; i < sharingMaxRowsPerHour+10; i++ {
		flood = append(flood, sharingObservation{
			InboundId: 1,
			IP:        fmt.Sprintf("10.0.%v.%v", i/256, i%256),
			Province:  "江苏",
		})
	}
	got := a.observe(base.Add(30*time.Second), flood, 30)

	found := false
	for _, f := range got {
		if f.IP == real.IP && f.ActiveSeconds == 60 {
			found = true
		}
	}
	if !found {
		t.Error("真实用户被扫描流量挤掉了")
	}
}
