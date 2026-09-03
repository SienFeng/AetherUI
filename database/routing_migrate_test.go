package database

import (
	"path/filepath"
	"testing"

	"a-ui/database/model"
)

func TestInitDBCreatesRoutingTables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := GetDB()
	for _, tbl := range []any{&model.DomainGroup{}, &model.OutboundNode{}, &model.RoutingRule{}} {
		if !db.Migrator().HasTable(tbl) {
			t.Errorf("table for %T was not created", tbl)
		}
	}
}

func TestOutboundNodeTagIsUnique(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := GetDB()
	first := &model.OutboundNode{Tag: "a-ui-hk", Remark: "HK", Protocol: "socks", Config: "{}", Enable: true}
	if err := db.Create(first).Error; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	dup := &model.OutboundNode{Tag: "a-ui-hk", Remark: "HK2", Protocol: "socks", Config: "{}", Enable: true}
	if err := db.Create(dup).Error; err == nil {
		t.Error("duplicate tag was accepted, want unique constraint violation")
	}
}

// 旧库里的规则只有 inbound_id。迁移必须把它搬到 inbound_ids，且
// 绝不改变任何一条规则的实际生效范围：inbound_id = 0 是「所有入站」，
// 对应新语义的空数组 []。
func TestMigrateRoutingRuleInboundIds(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := GetDB()

	// InboundId 字段已从结构体删除，AutoMigrate 不会再建这一列，所以要手工
	// 把它加回来，才能造出「旧库刚升级上来」的真实形态：有 inbound_id 的历史
	// 数据、有 AutoMigrate 新建的空 inbound_ids。
	if err := db.Exec("ALTER TABLE routing_rules ADD COLUMN inbound_id integer").Error; err != nil {
		t.Fatalf("simulate legacy column: %v", err)
	}

	insert := func(remark string, inboundId int, inboundIds string) {
		t.Helper()
		err := db.Exec(`INSERT INTO routing_rules
			(remark, inbound_id, inbound_ids, domain_group_id, action, outbound_id, priority, enable)
			VALUES (?, ?, ?, 1, 'block', 0, 0, 1)`, remark, inboundId, inboundIds).Error
		if err != nil {
			t.Fatalf("insert %s: %v", remark, err)
		}
	}
	insert("指定入站", 7, "")
	insert("全局规则", 0, "")
	insert("已迁移过", 7, "[1,2]")

	if err := migrateRoutingRuleInboundIds(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	read := func(remark string) string {
		t.Helper()
		var got string
		err := db.Raw("SELECT inbound_ids FROM routing_rules WHERE remark = ?", remark).Scan(&got).Error
		if err != nil {
			t.Fatalf("read %s: %v", remark, err)
		}
		return got
	}
	if got := read("指定入站"); got != "[7]" {
		t.Errorf("指定入站 inbound_ids = %q, want [7]", got)
	}
	if got := read("全局规则"); got != "[]" {
		t.Errorf("全局规则 inbound_ids = %q, want []", got)
	}
	// 已有值不能被覆盖，否则重启一次就把用户改过的多入站选择打回单选
	if got := read("已迁移过"); got != "[1,2]" {
		t.Errorf("已迁移过 inbound_ids = %q, want [1,2]", got)
	}

	// 幂等：面板每次启动都会跑这条迁移
	if err := migrateRoutingRuleInboundIds(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if got := read("指定入站"); got != "[7]" {
		t.Errorf("after second run: %q, want [7]", got)
	}
}

// 全新安装的库没有 inbound_id 列，迁移必须原地跳过而不是报 no such column。
// 这是 migrateRoutingRuleInboundIds 里那个 HasColumn 守卫存在的唯一理由：
// 没有它，每一次全新安装都会在 InitDB 阶段直接失败，面板根本起不来。
func TestMigrateRoutingRuleInboundIdsSkipsFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	// InitDB 内部已经跑过一次迁移；能走到这里就说明全新库没被它绊倒。
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB on a fresh database must not fail: %v", err)
	}
	if GetDB().Migrator().HasColumn(&model.RoutingRule{}, "inbound_id") {
		t.Fatal("a fresh database must not have the legacy inbound_id column")
	}
	if err := migrateRoutingRuleInboundIds(); err != nil {
		t.Errorf("migration must be a no-op on a fresh database, got %v", err)
	}
}
