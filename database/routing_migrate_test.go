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
