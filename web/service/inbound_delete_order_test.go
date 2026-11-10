package service

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"a-ui/database"
	"a-ui/database/model"
)

// DelInbound 的最后一步（删入站本身）失败时，什么都不该已经被删掉：改动前封禁在事务之前就删了，
// 入站还在、它的封禁名单却没了——被封的 IP 直接又能连上。
func TestDelInboundKeepsBansWhenTheDeleteFails(t *testing.T) {
	setupDB(t)
	in := makeInboundFor(t, 10061, func(*model.Inbound) {})
	if err := (&IPBanService{}).Ban(in.Id, "203.0.113.9", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	const name = "zz_test_fail_inbound_delete"
	if err := db.Callback().Delete().Before("gorm:delete").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "inbounds" {
			_ = tx.AddError(errors.New("模拟删除失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Delete().Remove(name)

	if err := (&InboundService{}).DelInbound(in.Id); err == nil {
		t.Fatal("模拟的删除失败没有报出来")
	}
	if got := reloadInbound(t, in.Id); got == nil {
		t.Fatal("入站不该被删掉")
	}
	bans, err := (&IPBanService{}).ActiveBans(in.Id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(bans) != 1 {
		t.Errorf("删除失败后封禁名单剩 %d 条，期望原样保留 1 条", len(bans))
	}
}
