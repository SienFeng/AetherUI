package job

import (
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/service"
)

// setupMeterJobDB 在 setupJobDB 之外再建一个用量库——计量池与域名分时桶都在那里。
func setupMeterJobDB(t *testing.T) {
	t.Helper()
	setupJobDB(t)
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
	// 用量库句柄是包级变量，会跨用例残留，而 SQLite 在文件被 t.TempDir 清掉
	// 之后仍能通过已打开的 fd 读到旧数据。
	t.Cleanup(database.ResetTrafficDBForTest)
	// 死计数器观测值也是包级变量：残留一个超限值会让 Recompute 走冻结分支
	// 直接返回，本用例什么都测不到。
	service.SetStaleMeterCounters(0)
	t.Cleanup(func() { service.SetStaleMeterCounters(0) })
}

// putJobDomainStat 写一行小时桶。桶时刻取自 time.Now()——Run 用的是真实时钟，
// 写死一个绝对时刻会让用例过了 24 小时窗口就失效。
func putJobDomainStat(t *testing.T, inboundId int, dom string) {
	t.Helper()
	row := &model.DomainStat{
		Granularity: model.GranularityHour, InboundId: inboundId,
		Domain: dom, BucketStart: time.Now().Add(-time.Hour).Unix(), Count: 10,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入域名统计: %v", err)
	}
}

// TestMeterPoolJobFlagsRestartEvenWhenRecomputeFails 钉住「池真的变了就一定置标志」。
//
// Recompute 逐入站累加 changed，任一入站出错就带着已经发生的变动 return。
// Run 若先看 err 再 return，这些变动就被丢掉了：库里的池已经换了，下发的配置
// 还停在旧池上，要等下一次无关改动或下一个整点才追上——中间那段时间，已退池
// 域名的计量出站还在，新进池的域名一个字节都收不到。
func TestMeterPoolJobFlagsRestartEvenWhenRecomputeFails(t *testing.T) {
	setupMeterJobDB(t)
	in := makeJobInbound(t, func(in *model.Inbound) { in.Enable = true })
	putJobDomainStat(t, in.Id, "alpha.com")
	putJobDomainStat(t, in.Id, "beta.com")

	// 让第二个域名进池时写库失败，构造「已经改了一部分、然后出错」这个状态。
	// 新池按域名升序写入，所以 alpha.com 会先建成功（changed = 1），beta.com
	// 才炸。触发器是这里唯一能精确制造「中途失败」的手段。
	err := database.GetTrafficDB().Exec(
		`CREATE TRIGGER meter_insert_boom BEFORE INSERT ON meter_domains
		 WHEN NEW.domain = 'beta.com'
		 BEGIN SELECT RAISE(ABORT, 'boom'); END`).Error
	if err != nil {
		t.Fatalf("建触发器: %v", err)
	}

	NewMeterPoolJob().Run()

	// 先确认这一轮确实是「改了一部分又出错」，否则下面那条断言测的是别的东西。
	var rows []model.MeterDomain
	if err := database.GetTrafficDB().Where("inbound_id = ?", in.Id).
		Order("domain asc").Find(&rows).Error; err != nil {
		t.Fatalf("查询池行: %v", err)
	}
	if len(rows) != 1 || rows[0].Domain != "alpha.com" {
		t.Fatalf("池行 = %+v，期望只有 alpha.com（beta.com 写入必须失败），"+
			"用例的前提不成立", rows)
	}

	if !(&service.XrayService{}).IsNeedRestartAndSetFalse() {
		t.Error("池已经变了却没有置重启标志——下发的配置会一直停在旧池上")
	}
}

// 池没变就不该置标志：白置只会让那个 10 秒的消费任务空跑一次 RestartXray。
func TestMeterPoolJobDoesNotFlagRestartWhenPoolIsUnchanged(t *testing.T) {
	setupMeterJobDB(t)
	makeJobInbound(t, func(in *model.Inbound) { in.Enable = true })

	// 没有任何域名统计 → 池重算前后都是空。
	NewMeterPoolJob().Run()

	if (&service.XrayService{}).IsNeedRestartAndSetFalse() {
		t.Error("池没有任何变化也置了重启标志")
	}
}
