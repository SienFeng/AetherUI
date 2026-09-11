package database

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"a-ui/database/model"
)

// 这两条测试守的是「共享检测的两个全扫」这个已修问题，不是在测 GORM。
//
// 必须成对存在，因为两种失效方式完全不同、互相看不见：
//
//   - 索引 tag 写错（写成 index:idx_x,priorty:1 这种拼写错误）是**静默**的：
//     GORM 不报错，AutoMigrate 照常成功，索引就是不生成。只有 HasIndex 看得见。
//   - 索引建出来了但查询用不上（比如将来有人给 windowRows 加了个会让
//     hour_start 失去可搜索性的条件），HasIndex 照常通过。只有查询计划看得见。
func openTrafficDBForTest(t *testing.T) {
	t.Helper()
	ResetTrafficDBForTest()
	t.Cleanup(ResetTrafficDBForTest)
	if err := InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
}

func TestInboundIPHourHasQueryIndexes(t *testing.T) {
	openTrafficDBForTest(t)

	m := GetTrafficDB().Migrator()
	for _, name := range []string{"idx_iph_inbound_hour", "idx_iph_hour"} {
		if !m.HasIndex(&model.InboundIPHour{}, name) {
			t.Errorf("索引 %s 没有建出来——检查 model.InboundIPHour 的 gorm tag，写错是静默的", name)
		}
	}
	// 唯一索引必须原样保留：它承担的是 upsertIPHour 的冲突判定，不是查询加速。
	if !m.HasIndex(&model.InboundIPHour{}, "idx_inbound_ip_hour") {
		t.Error("原有唯一索引 idx_inbound_ip_hour 丢失")
	}
}

// 查询计划里出现 SCAN 而不是 SEARCH…USING INDEX，就是退回全表扫了。
func TestInboundIPHourWindowQueriesUseIndex(t *testing.T) {
	openTrafficDBForTest(t)
	db := GetTrafficDB()

	// 空表上 SQLite 仍可能选全扫，塞一批行让计划稳定。
	base := model.AlignHourUTC(time.Now())
	rows := make([]model.InboundIPHour, 0, 400)
	for inbound := 1; inbound <= 10; inbound++ {
		for h := 0; h < 40; h++ {
			rows = append(rows, model.InboundIPHour{
				InboundId: inbound,
				IP:        fmt.Sprintf("10.0.%d.%d", inbound, h),
				HourStart: base - int64(h)*3600,
				Province:  "江苏",
			})
		}
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("填充数据: %v", err)
	}

	cases := []struct {
		name  string
		sql   string
		args  []any
		index string
	}{{
		// SharingService.Summary：不带入站条件，每次加载入站列表都跑。
		name:  "Summary 的全库取窗口",
		sql:   "SELECT * FROM inbound_ip_hours WHERE hour_start >= ?",
		args:  []any{base - 7*24*3600},
		index: "idx_iph_hour",
	}, {
		// SharingService.Detail：按入站取窗口。
		name:  "Detail 的按入站取窗口",
		sql:   "SELECT * FROM inbound_ip_hours WHERE hour_start >= ? AND inbound_id = ?",
		args:  []any{base - 7*24*3600, 3},
		index: "idx_iph_inbound_hour",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var plan []struct {
				Detail string
			}
			err := db.Raw("EXPLAIN QUERY PLAN "+c.sql, c.args...).Scan(&plan).Error
			if err != nil {
				t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
			}
			var lines []string
			for _, p := range plan {
				lines = append(lines, p.Detail)
			}
			joined := strings.Join(lines, " | ")
			if !strings.Contains(joined, c.index) {
				t.Errorf("查询没有走 %s，计划是：%s", c.index, joined)
			}
		})
	}
}

// 所有现存部署都是已经带着 30 天数据的老库，索引必须由 AutoMigrate 补建。
// 补不上的话这个修复对谁都不生效，而且完全静默——面板照常工作，只是每次
// 加载入站列表仍然全表扫。
func TestInboundIPHourIndexesAreAddedToExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "traffic.db")

	ResetTrafficDBForTest()
	t.Cleanup(ResetTrafficDBForTest)
	if err := InitTrafficDB(path); err != nil {
		t.Fatalf("首次 InitTrafficDB: %v", err)
	}
	// 退回「加索引之前」的库形态。
	for _, name := range []string{"idx_iph_inbound_hour", "idx_iph_hour"} {
		if err := GetTrafficDB().Exec("DROP INDEX " + name).Error; err != nil {
			t.Fatalf("DROP INDEX %s: %v", name, err)
		}
	}
	row := model.InboundIPHour{
		InboundId: 7, IP: "1.2.3.4", HourStart: model.AlignHourUTC(time.Now()),
		Province: "江苏", ActiveSeconds: 1800, ActiveBytes: 5 << 20,
	}
	if err := GetTrafficDB().Create(&row).Error; err != nil {
		t.Fatalf("写入既有数据: %v", err)
	}

	// 升级：面板重启，AutoMigrate 再跑一次。
	ResetTrafficDBForTest()
	if err := InitTrafficDB(path); err != nil {
		t.Fatalf("升级后 InitTrafficDB: %v", err)
	}
	m := GetTrafficDB().Migrator()
	for _, name := range []string{"idx_iph_inbound_hour", "idx_iph_hour"} {
		if !m.HasIndex(&model.InboundIPHour{}, name) {
			t.Errorf("升级后索引 %s 仍未建出来，存量部署拿不到这个修复", name)
		}
	}
	var count int64
	if err := GetTrafficDB().Model(&model.InboundIPHour{}).Count(&count).Error; err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Errorf("升级后既有数据行数 = %d，want 1", count)
	}
}
