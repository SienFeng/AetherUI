package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"a-ui/database"
	"a-ui/database/model"
)

// observeAcked 模拟「每一批都写库成功」：observe 之后立刻确认。累加器的落库节奏
// （满 60 秒才写、写过之后再满 60 秒才写）是在写库成功的前提下定义的。
func observeAcked(a *sharingAccumulator, now time.Time, obs []sharingObservation, step int) []sharingFlush {
	out := a.observe(now, obs, step)
	a.ack(out, now)
	return out
}

// failIPHourWrites 让之后对 inbound_ip_hours 的写入失败（读照常），返回恢复函数。
// 模拟的是「库被锁 / 盘满」这类只坏写入的故障。
func failIPHourWrites(t *testing.T, db *gorm.DB) (restore func()) {
	t.Helper()
	const name = "test:fail-ip-hour-writes"
	if err := db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "inbound_ip_hours" {
			_ = tx.AddError(errors.New("模拟写库失败: database is locked"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	removed := false
	restore = func() {
		if !removed {
			removed = true
			_ = db.Callback().Create().Remove(name)
		}
	}
	t.Cleanup(restore)
	return restore
}

func ipHourAt(t *testing.T, inboundId int, ip string, hour int64) (model.InboundIPHour, bool) {
	t.Helper()
	var rows []model.InboundIPHour
	if err := database.GetTrafficDB().Where("inbound_id = ? AND ip = ? AND hour_start = ?", inboundId, ip, hour).
		Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return model.InboundIPHour{}, false
	}
	return rows[0], true
}

// 落库是覆盖式的绝对值（upsertIPHour）。面板重启后累加器从零开始，新进程第一次写这一行
// 就会把重启前写下的整小时数据覆盖掉——实测一行从 911,212,544 字节 / 2370 秒变成了
// 11,534,336 字节 / 30 秒。修复：一个小时里第一次建 cell 时，先拿库里已有的那一行打底。
func TestRecordSharingSeedsCellFromRowWrittenBeforeRestart(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()
	hour := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	meta := NetworkMeta{Country: "中国", Province: "江苏省", City: "南京市", ISP: "中国电信"}
	if err := db.Create(&model.InboundIPHour{
		InboundId: 1, IP: "192.0.2.5", HourStart: hour.Unix(),
		Country: meta.Country, Province: meta.Province, City: meta.City, ISP: meta.ISP,
		IdentityVersion: sharingIdentityVersion,
		ActiveSeconds:   2370, ActiveBytes: 911_212_544, ActiveUp: 11_212_544, ActiveDown: 900_000_000,
	}).Error; err != nil {
		t.Fatal(err)
	}

	acc := newSharingAccumulator() // 重启后的新进程
	obs := []sharingObservation{{InboundId: 1, IP: "192.0.2.5", Meta: meta, Up: 1_534_336, Down: 10_000_000}}
	if err := recordSharing(db, acc, hour.Add(40*time.Minute), func() []sharingObservation { return obs }); err != nil {
		t.Fatal(err)
	}

	row, ok := ipHourAt(t, 1, "192.0.2.5", hour.Unix())
	if !ok {
		t.Fatal("行不见了")
	}
	if row.ActiveSeconds != 2400 || row.ActiveBytes != 922_746_880 ||
		row.ActiveUp != 12_746_880 || row.ActiveDown != 910_000_000 {
		t.Fatalf("重启后这一行 = %d 秒 / %d 字节（上 %d 下 %d），期望在重启前的值上接着累加：2400 秒 / 922746880 字节",
			row.ActiveSeconds, row.ActiveBytes, row.ActiveUp, row.ActiveDown)
	}
}

// 写库失败的那一批必须重写。修复前 cell 在写库之前就被记成「已落库」，失败的那一批要
// 再等满一个门槛才有机会写，跨小时的那一次收尾写入则永远丢了。
func TestRecordSharingRetriesWriteAfterFailure(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()
	acc := newSharingAccumulator()
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	collect := func() []sharingObservation {
		return []sharingObservation{{InboundId: 1, IP: "192.0.2.5", Meta: provinceMeta("江苏")}}
	}

	if err := recordSharing(db, acc, base, collect); err != nil {
		t.Fatal(err)
	}
	restore := failIPHourWrites(t, db)
	if err := recordSharing(db, acc, base.Add(30*time.Second), collect); err == nil {
		t.Fatal("满 60 秒的那一批写库失败，recordSharing 应报错")
	}
	restore()
	if err := recordSharing(db, acc, base.Add(60*time.Second), collect); err != nil {
		t.Fatal(err)
	}
	row, ok := ipHourAt(t, 1, "192.0.2.5", model.AlignHourUTC(base))
	if !ok || row.ActiveSeconds != 90 {
		t.Fatalf("写库恢复后的这一轮应当把失败的那一批补上（90 秒），实际 %+v（存在=%v）", row, ok)
	}
}

func TestRecordSharingRetriesRolloverWriteAfterFailure(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()
	acc := newSharingAccumulator()
	h10 := time.Date(2026, 11, 9, 10, 58, 30, 0, time.UTC)
	h11 := time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC)
	collect := func() []sharingObservation {
		return []sharingObservation{{InboundId: 1, IP: "192.0.2.5", Meta: provinceMeta("江苏")}}
	}

	for _, at := range []time.Time{h10, h10.Add(30 * time.Second), h10.Add(60 * time.Second)} {
		if err := recordSharing(db, acc, at, collect); err != nil {
			t.Fatal(err)
		}
	}
	// 10 点那一行此刻是 60 秒，最后 30 秒还在内存里，靠跨小时的收尾写入补上。
	restore := failIPHourWrites(t, db)
	if err := recordSharing(db, acc, h11, collect); err == nil {
		t.Fatal("收尾写入失败，recordSharing 应报错")
	}
	restore()
	if err := recordSharing(db, acc, h11.Add(30*time.Second), collect); err != nil {
		t.Fatal(err)
	}
	row, ok := ipHourAt(t, 1, "192.0.2.5", model.AlignHourUTC(h10))
	if !ok || row.ActiveSeconds != 90 {
		t.Fatalf("10 点那一行 = %+v（存在=%v），期望失败的收尾写入在下一轮补上、记满 90 秒", row, ok)
	}
}

// 没确认落库的一批，下一轮照样再发一次（带上最新的绝对值），直到确认为止。
func TestAccumulatorReemitsFlushUntilAcknowledged(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	a.observe(base, oneObs(), 30)
	if got := a.observe(base.Add(30*time.Second), oneObs(), 30); len(got) != 1 || got[0].ActiveSeconds != 60 {
		t.Fatalf("满 60 秒 = %+v, want 1 条 60 秒", got)
	}
	// 上一批没确认（写库失败）：这一轮必须再发，带上最新的 90 秒。
	got := a.observe(base.Add(60*time.Second), oneObs(), 30)
	if len(got) != 1 || got[0].ActiveSeconds != 90 {
		t.Fatalf("未确认的一批没有重发: %+v", got)
	}
	a.ack(got, base.Add(60*time.Second))
	if got := a.observe(base.Add(90*time.Second), oneObs(), 30); len(got) != 0 {
		t.Fatalf("确认之后距上次落库只多 30 秒，不该再写: %+v", got)
	}
}

func TestAccumulatorReemitsRolloverFlushUntilAcknowledged(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	observeAcked(a, h10, oneObs(), 30)
	observeAcked(a, h10.Add(30*time.Second), oneObs(), 30) // 60，已落库
	observeAcked(a, h10.Add(60*time.Second), oneObs(), 30) // 90，未到下一次落库

	first := a.observe(h11, nil, 30)
	if len(first) != 1 || first[0].HourStart != model.AlignHourUTC(h10) || first[0].ActiveSeconds != 90 {
		t.Fatalf("跨小时收尾 = %+v, want 10 点那一行 90 秒", first)
	}
	again := a.observe(h11.Add(30*time.Second), nil, 30)
	if len(again) != 1 || again[0].HourStart != model.AlignHourUTC(h10) || again[0].ActiveSeconds != 90 {
		t.Fatalf("收尾写入没确认，下一轮应当重发: %+v", again)
	}
	a.ack(again, h11.Add(30*time.Second))
	if got := a.observe(h11.Add(60*time.Second), nil, 30); len(got) != 0 {
		t.Fatalf("确认之后不该再发上一小时的收尾: %+v", got)
	}
}

// 库里本小时已有的行同样占着 sharingMaxRowsPerHour 的名额：重启之后不能再放进 50 个新来源。
func TestAccumulatorSeededRowsCountTowardHourlyCap(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	hour := model.AlignHourUTC(base)
	rows := make([]model.InboundIPHour, 0, sharingMaxRowsPerHour)
	for i := 0; i < sharingMaxRowsPerHour; i++ {
		rows = append(rows, model.InboundIPHour{InboundId: 1, IP: fmt.Sprintf("198.51.100.%d", i), HourStart: hour, ActiveSeconds: 60})
	}
	a.seed(hour, rows)

	fresh := []sharingObservation{{InboundId: 1, IP: "203.0.113.9", Meta: provinceMeta("江苏")}}
	observeAcked(a, base, fresh, 30)
	if got := observeAcked(a, base.Add(30*time.Second), fresh, 30); len(got) != 0 {
		t.Fatalf("本小时库里已有 %d 行，新来源仍然落库了: %+v", sharingMaxRowsPerHour, got)
	}
}

// 库里已有这一行的来源，纯字节观测直接接着那一行记，不再进暂存等 1 MB 门槛——
// 门槛早在这一行第一次落库时就过了。
func TestAccumulatorBytesOnlyForSeededSourceContinuesTheRow(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 20, 0, 0, time.UTC)
	hour := model.AlignHourUTC(base)
	a.seed(hour, []model.InboundIPHour{{
		InboundId: 1, IP: "192.0.2.5", HourStart: hour, Province: "江苏", Country: "中国",
		ActiveSeconds: 600, ActiveBytes: 5 << 20, ActiveUp: 1 << 20, ActiveDown: 4 << 20,
	}})

	// 这一轮没查到归属地（Meta 为零值）。
	got := observeAcked(a, base, []sharingObservation{{InboundId: 1, IP: "192.0.2.5", Up: 1000, BytesOnly: true}}, 30)
	if len(got) != 1 || got[0].ActiveSeconds != 600 || got[0].ActiveBytes != 5<<20+1000 || got[0].ActiveUp != 1<<20+1000 {
		t.Fatalf("落库 = %+v, want 在库里那一行上接着记（600 秒、%d 字节）", got, 5<<20+1000)
	}
	if got[0].Meta.Province != "江苏" {
		t.Errorf("画像 = %+v，这一轮没查到归属地时应沿用库里那一行的", got[0].Meta)
	}
}

// 删除入站时，库里打底的行与没确认落库的收尾写入都要一起丢掉：SQLite 会复用自增 id，
// 留着的话它们会被写回、绑到下一个建出来的入站上。
func TestAccumulatorForgetDropsSeedsAndUnconfirmedRollover(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	observeAcked(a, h10, oneObs(), 30)
	observeAcked(a, h10.Add(30*time.Second), oneObs(), 30)
	observeAcked(a, h10.Add(60*time.Second), oneObs(), 30)
	a.observe(h11, nil, 30) // 收尾写入发出、没确认
	a.seed(model.AlignHourUTC(h11), []model.InboundIPHour{{InboundId: 1, IP: "192.0.2.5", HourStart: model.AlignHourUTC(h11), ActiveSeconds: 900}})

	a.forget(1)

	if got := a.observe(h11.Add(30*time.Second), nil, 30); len(got) != 0 {
		t.Fatalf("已删除的入站 1 的收尾写入又被发出来了: %+v", got)
	}
	got := observeAcked(a, h11.Add(60*time.Second), []sharingObservation{{InboundId: 1, IP: "192.0.2.5", Meta: provinceMeta("江苏"), Up: 2 << 20}}, 30)
	for _, f := range got {
		if f.ActiveSeconds >= 900 {
			t.Fatalf("已删除的入站 1 的打底行被接着记了: %+v", f)
		}
	}
}
