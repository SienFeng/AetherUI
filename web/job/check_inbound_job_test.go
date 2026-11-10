package job

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/service"
)

// DisableInvalidInbounds 拆成两条 UPDATE：第二条（超流量）失败时，第一条（到期）停掉的入站
// 已经落库，返回值里的条数就是它停掉的。只在 err == nil 时置重启标志的话，这些入站在库里
// 已经停用、核心里却继续跑到下一次别的改动——到期的用户照常能用。
func TestCheckInboundJobFlagsRestartOnPartialSuccess(t *testing.T) {
	setupJobDB(t)
	past := time.Now().Add(-time.Hour).UnixMilli()
	expired := makeJobInbound(t, func(in *model.Inbound) {
		in.Enable, in.ExpiryTime = true, past
	})
	makeJobInbound(t, func(in *model.Inbound) {
		in.Port, in.Tag = 10012, "inbound-10012"
		in.Enable, in.Total, in.Up, in.Down = true, 100, 60, 60
	})
	// 让超流量那条（会写 disabled_by_traffic）失败，到期那条照常成功。
	db := database.GetDB()
	if err := db.Exec(`CREATE TRIGGER fail_over_quota BEFORE UPDATE OF disabled_by_traffic ON inbounds
		BEGIN SELECT RAISE(ABORT, 'database is locked (simulated)'); END`).Error; err != nil {
		t.Fatalf("建触发器: %v", err)
	}

	NewCheckInboundJob().Run()

	var got model.Inbound
	if err := db.First(&got, expired.Id).Error; err != nil {
		t.Fatal(err)
	}
	if got.Enable {
		t.Fatal("前提不成立：到期那条 UPDATE 应当已经生效")
	}
	if !(&service.XrayService{}).IsNeedRestartAndSetFalse() {
		t.Error("到期入站已在库里停用，却因为第二条 UPDATE 失败而没有置重启标志")
	}
}
