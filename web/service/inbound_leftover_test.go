package service

import (
	"path/filepath"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// TestAddInboundClearsLeftoversOfReusedId 复现审查里的 R4-M4：删除入站时清理其它库失败（只告警、由每小时的
// PruneOrphans 兜底），而这一小时内新建的入站拿到了同一个 id（SQLite 复用自增 id）——残留行从此不再是孤儿，
// 新用户继承上一个用户的按来源用量，自动设置地区会拿它来判断这个新用户在哪个省。
func TestAddInboundClearsLeftoversOfReusedId(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "main.db")); err != nil {
		t.Fatal(err)
	}
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatal(err)
	}
	if err := database.InitAccessLogDB(filepath.Join(t.TempDir(), "access.db")); err != nil {
		t.Fatal(err)
	}
	s := InboundService{}
	old := &model.Inbound{UserId: 1, Port: 20111, Protocol: model.VLESS, Tag: "inbound-20111", Enable: true,
		Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}"}
	if err := s.AddInbound(old); err != nil {
		t.Fatal(err)
	}
	// 只删入站本身，模拟删除时其它库的清理失败留下的残留。
	if err := database.GetDB().Delete(&model.Inbound{}, old.Id).Error; err != nil {
		t.Fatal(err)
	}
	tdb, adb := database.GetTrafficDB(), database.GetAccessLogDB()
	if err := tdb.Create(&model.InboundIPHour{InboundId: old.Id, IP: "192.0.2.9", HourStart: 1_800_000_000, Province: "江苏省",
		ActiveSeconds: 1800, ActiveBytes: 50 << 20}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tdb.Create(&model.TrafficBucket{InboundId: old.Id, Granularity: model.GranularityHour, BucketStart: 1_800_000_000, Up: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := adb.Create(&model.AccessLog{InboundId: old.Id, Time: 1_800_000_000_000, SourceIP: "192.0.2.9", Route: "direct", Accepted: true}).Error; err != nil {
		t.Fatal(err)
	}

	fresh := &model.Inbound{UserId: 1, Port: 20112, Protocol: model.VLESS, Tag: "inbound-20112", Enable: true,
		Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}"}
	if err := s.AddInbound(fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Id != old.Id {
		t.Skipf("这个 SQLite 没有复用 id（%d → %d），前提不成立", old.Id, fresh.Id)
	}
	var n int64
	tdb.Model(&model.InboundIPHour{}).Where("inbound_id = ?", fresh.Id).Count(&n)
	if n != 0 {
		t.Errorf("新入站继承了上一个用户的按来源用量 %d 行", n)
	}
	tdb.Model(&model.TrafficBucket{}).Where("inbound_id = ?", fresh.Id).Count(&n)
	if n != 0 {
		t.Errorf("新入站继承了上一个用户的用量历史 %d 行", n)
	}
	adb.Model(&model.AccessLog{}).Where("inbound_id = ?", fresh.Id).Count(&n)
	if n != 0 {
		t.Errorf("新入站继承了上一个用户的访问记录 %d 行", n)
	}
}
