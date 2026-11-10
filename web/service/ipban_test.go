package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

func TestBanIsActiveUntilItExpires(t *testing.T) {
	setupDB(t)
	svc := IPBanService{}
	now := time.Now()

	if err := svc.Ban(1, "1.1.1.1", 60*time.Second, now); err != nil {
		t.Fatalf("Ban: %v", err)
	}

	bans, err := svc.ActiveBans(1, now.Add(59*time.Second))
	if err != nil {
		t.Fatalf("ActiveBans: %v", err)
	}
	if len(bans) != 1 || bans[0].IP != "1.1.1.1" {
		t.Fatalf("59s 时封禁应仍生效: %+v", bans)
	}

	bans, err = svc.ActiveBans(1, now.Add(61*time.Second))
	if err != nil {
		t.Fatalf("ActiveBans: %v", err)
	}
	if len(bans) != 0 {
		t.Errorf("61s 时封禁应已过期: %+v", bans)
	}
}

// duration <= 0 表示永久封禁，不会随时间自动解除。
func TestBanWithNonPositiveDurationIsPermanent(t *testing.T) {
	setupDB(t)
	svc := IPBanService{}
	now := time.Now()

	if err := svc.Ban(1, "2.2.2.2", 0, now); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	bans, err := svc.ActiveBans(1, now.Add(10*365*24*time.Hour))
	if err != nil {
		t.Fatalf("ActiveBans: %v", err)
	}
	if len(bans) != 1 {
		t.Fatalf("永久封禁十年后仍应生效: %+v", bans)
	}
	if bans[0].ExpiresAt != 0 {
		t.Errorf("ExpiresAt = %d, want 0（0 表示永久）", bans[0].ExpiresAt)
	}
}

// 重复封禁同一个 IP 只更新到期时间，不堆积多行——否则解封要删几行才算完，
// 界面也会出现同一个 IP 的多条记录。
func TestBanTwiceUpdatesExpiryInPlace(t *testing.T) {
	setupDB(t)
	svc := IPBanService{}
	now := time.Now()

	if err := svc.Ban(1, "3.3.3.3", 60*time.Second, now); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if err := svc.Ban(1, "3.3.3.3", 600*time.Second, now); err != nil {
		t.Fatalf("Ban again: %v", err)
	}

	var count int64
	if err := database.GetDB().Model(&model.IPBan{}).Where("inbound_id = ? and ip = ?", 1, "3.3.3.3").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("同一个 IP 的封禁行数 = %d, want 1", count)
	}
	bans, _ := svc.ActiveBans(1, now.Add(300*time.Second))
	if len(bans) != 1 {
		t.Errorf("延长后 300s 时应仍生效: %+v", bans)
	}
}

func TestUnbanRemovesIt(t *testing.T) {
	setupDB(t)
	svc := IPBanService{}
	now := time.Now()
	if err := svc.Ban(1, "4.4.4.4", 0, now); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if err := svc.Unban(1, "4.4.4.4"); err != nil {
		t.Fatalf("Unban: %v", err)
	}
	bans, _ := svc.ActiveBans(1, now)
	if len(bans) != 0 {
		t.Errorf("解封后不应还有封禁: %+v", bans)
	}
}

// 封禁按入站隔离：封了入站 1 的 IP 不该影响入站 2 的同一个 IP。
func TestBanIsScopedToInbound(t *testing.T) {
	setupDB(t)
	svc := IPBanService{}
	now := time.Now()
	if err := svc.Ban(1, "5.5.5.5", 0, now); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	bans, _ := svc.ActiveBans(2, now)
	if len(bans) != 0 {
		t.Errorf("入站 2 不该看到入站 1 的封禁: %+v", bans)
	}
}

// SQLite 会复用被删除的自增 id：入站删掉后不清封禁，下一个建出来的入站
// 会凭空继承上一个用户的封禁名单。
func TestDeleteByInboundClearsBans(t *testing.T) {
	setupDB(t)
	svc := IPBanService{}
	now := time.Now()
	if err := svc.Ban(7, "6.6.6.6", 0, now); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if err := svc.DeleteByInbound(7); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}
	bans, _ := svc.ActiveBans(7, now)
	if len(bans) != 0 {
		t.Errorf("入站删除后其封禁应被清空: %+v", bans)
	}
}

// 过期的封禁行要能被清掉，否则库里会无限堆积。
func TestPruneExpiredRemovesOnlyExpired(t *testing.T) {
	setupDB(t)
	svc := IPBanService{}
	now := time.Now()
	if err := svc.Ban(1, "7.7.7.7", 10*time.Second, now); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if err := svc.Ban(1, "8.8.8.8", 0, now); err != nil { // 永久
		t.Fatalf("Ban: %v", err)
	}
	deleted, err := svc.PruneExpired(now.Add(time.Minute))
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if deleted != 1 {
		t.Errorf("清理行数 = %d, want 1", deleted)
	}
	bans, _ := svc.ActiveBans(1, now.Add(time.Minute))
	if len(bans) != 1 || bans[0].IP != "8.8.8.8" {
		t.Errorf("永久封禁不该被清掉: %+v", bans)
	}
}

// 被封禁的 IP 不参与并发额度判定：它的连接每轮都会被断开，
// 让它占着名额会把额度白白吃掉。
func TestPlanRejectionsIgnoresBanned(t *testing.T) {
	base := time.Now().UnixMilli()
	list := []OnlineIP{
		{IP: "1.1.1.1", FirstSeen: base, Conns: 3, Banned: true},
		{IP: "2.2.2.2", FirstSeen: base + 1000, Conns: 1},
	}
	over := planRejections(list, 1)
	if len(over) != 0 {
		t.Errorf("被拒集合 = %v, want 空（被封禁的 1.1.1.1 不占额度）", over)
	}
}

// 没设并发额度、但有活跃封禁的入站也必须进入判定，否则封禁对它永远不会执行。
func TestLimitedInboundsIncludesBannedOnlyInbound(t *testing.T) {
	setupDB(t)
	db := database.GetDB()
	in := &model.Inbound{
		UserId: 1, Port: 31001, Protocol: model.VLESS, Enable: true,
		Tag: "inbound-31001", ConcurrencyLimit: 0,
		Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
	}
	if err := db.Create(in).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}

	s := ConcurrencyService{}
	now := time.Now()
	got, err := s.limitedInbounds(now)
	if err != nil {
		t.Fatalf("limitedInbounds: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("没额度也没封禁时不该进判定: %+v", got)
	}

	if err := (&IPBanService{}).Ban(in.Id, "9.9.9.9", time.Hour, now); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	got, err = s.limitedInbounds(now)
	if err != nil {
		t.Fatalf("limitedInbounds: %v", err)
	}
	if len(got) != 1 || got[0].Id != in.Id {
		t.Fatalf("有封禁后应进判定: %+v", got)
	}

	// 封禁过期后又该退出判定，否则一条早就失效的封禁会让这个入站被永久轮询。
	got, err = s.limitedInbounds(now.Add(2 * time.Hour))
	if err != nil {
		t.Fatalf("limitedInbounds: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("封禁过期后不该再进判定: %+v", got)
	}
}

// 封禁来源即使连接已被断干净（不在连接表里），也必须出现在列表里，
// 否则管理员看不到自己封了谁，也就无从解封。
func TestMarkBannedAddsSourcesWithNoConnections(t *testing.T) {
	list := []OnlineIP{{IP: "1.1.1.1", Conns: 2}}
	banned := map[string]*model.IPBan{
		"9.9.9.9": {IP: "9.9.9.9", ExpiresAt: 0},
	}
	got := markBanned(list, banned)
	if len(got) != 2 {
		t.Fatalf("条目数 = %d, want 2: %+v", len(got), got)
	}
	var found *OnlineIP
	for i := range got {
		if got[i].IP == "9.9.9.9" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("没有连接的封禁来源未出现在列表里: %+v", got)
	}
	if !found.Banned || found.Conns != 0 {
		t.Errorf("封禁条目 = %+v, want Banned=true Conns=0", *found)
	}
}
