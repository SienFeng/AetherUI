package service

import (
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// oldInboundsDDL 是本次改动之前 inbounds 表的样子（字段清单取自
// git show HEAD~:database/model/model.go）。用裸 SQL 而不是让 GORM 按一个
// 影子结构体建表：影子结构体写错了测试会跟着一起错，而这里要验的恰恰是
// 「真实的老库能不能升上来」。
const oldInboundsDDL = `
CREATE TABLE inbounds (
	id integer PRIMARY KEY AUTOINCREMENT,
	user_id integer,
	up integer,
	down integer,
	total integer,
	remark text,
	enable numeric,
	expiry_time integer,
	regions text,
	up_mbit integer,
	down_mbit integer,
	concurrency_limit integer,
	listen text,
	port integer UNIQUE,
	protocol text,
	settings text,
	stream_settings text,
	tag text UNIQUE,
	sniffing text
)`

// 升级路径：老库里已有的入站，AutoMigrate 加上三列之后必须一切照旧——
// 数据不动、三列都是零值、而零值恰好等于「不重置」，所以 TrafficResetJob
// 不会碰任何人的流量。
//
// 这是推送前最大的一条风险：如果新列的默认值不是零值，或者 AutoMigrate
// 在这张表上失败，后果是所有存量用户的已用流量在升级后某一刻被清零，
// 而面板不会有任何提示。
func TestUpgradeFromOldSchemaLeavesExistingInboundsUntouched(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")

	// 1. 先造一个改动之前的库，并塞一条已经用掉一半流量的入站。
	old, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("打开老库: %v", err)
	}
	if err := old.Exec(oldInboundsDDL).Error; err != nil {
		t.Fatalf("建老表: %v", err)
	}
	if err := old.Exec(`INSERT INTO inbounds
		(id, user_id, up, down, total, remark, enable, expiry_time, regions,
		 up_mbit, down_mbit, concurrency_limit, listen, port, protocol,
		 settings, stream_settings, tag, sniffing)
		VALUES (1, 1, 5000, 7000, 100000, '老用户', 1, 0, '[]',
		        0, 0, 0, '', 10011, 'vmess',
		        '{"clients":[{"id":"x"}]}', '{"network":"tcp"}', 'inbound-10011', '{}')`).Error; err != nil {
		t.Fatalf("插入老数据: %v", err)
	}
	sqlDB, err := old.DB()
	if err != nil {
		t.Fatalf("取底层连接: %v", err)
	}
	sqlDB.Close()

	// 2. 用当前代码打开同一个库，走真实的 InitDB / AutoMigrate。
	if err := database.InitDB(dbPath); err != nil {
		t.Fatalf("升级老库失败: %v", err)
	}

	got, err := (&InboundService{}).GetInbound(1)
	if err != nil {
		t.Fatalf("升级后读不到老入站: %v", err)
	}

	// 3. 老数据一个字节都不能变。
	if got.Up != 5000 || got.Down != 7000 || got.Total != 100000 {
		t.Errorf("老数据被改了: up=%d down=%d total=%d", got.Up, got.Down, got.Total)
	}
	if got.Remark != "老用户" || !got.Enable {
		t.Errorf("老数据被改了: remark=%q enable=%v", got.Remark, got.Enable)
	}

	// 4. 三个新列必须是零值，而零值就是「关闭」。
	if got.TrafficResetMode != model.TrafficResetOff {
		t.Errorf("TrafficResetMode = %d，升级后必须是 %d（不重置）",
			got.TrafficResetMode, model.TrafficResetOff)
	}
	if got.DisabledByTraffic {
		t.Error("DisabledByTraffic 升级后必须是 false")
	}
	if got.LastResetAt != 0 {
		t.Errorf("LastResetAt = %d，升级后必须是 0", got.LastResetAt)
	}

	// 5. 于是这一轮重置什么都不该做——这就是「升级后行为零变化」。
	reset, reEnabled, err := (&InboundService{}).ResetDueTraffic(time.Now(), time.UTC)
	if err != nil {
		t.Fatalf("ResetDueTraffic: %v", err)
	}
	if reset != 0 || reEnabled != 0 {
		t.Errorf("升级后第一轮就动了存量入站: reset=%d reEnabled=%d", reset, reEnabled)
	}
	if after := reloadInbound(t, 1); after.Up != 5000 || after.Down != 7000 {
		t.Errorf("存量入站的流量被清零了: up=%d down=%d", after.Up, after.Down)
	}
}
