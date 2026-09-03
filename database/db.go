package database

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"io/fs"
	"os"
	"path"
	"a-ui/config"
	"a-ui/database/model"
)

var db *gorm.DB

func initUser() error {
	err := db.AutoMigrate(&model.User{})
	if err != nil {
		return err
	}
	var count int64
	err = db.Model(&model.User{}).Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		user := &model.User{
			Username: "admin",
			Password: "admin",
		}
		return db.Create(user).Error
	}
	return nil
}

func initInbound() error {
	return db.AutoMigrate(&model.Inbound{})
}

func initSetting() error {
	return db.AutoMigrate(&model.Setting{})
}

// migrateRoutingRuleInboundIds 把旧的单入站字段 inbound_id 搬到 inbound_ids。
//
// 幂等：只回填 inbound_ids 为空的行，面板每次启动都会跑，重启多少次都安全。
//
// inbound_id = 0 在旧语义里是「所有入站」，对应新语义的空数组 []——两者
// 生效范围完全一致，迁移不改变任何一条规则的实际行为。
//
// 全新安装的库没有 inbound_id 列（该字段最终会从结构体删除），直接执行会
// 报 no such column，所以先探测。字段不在结构体上时 GORM 会把传入的字符串
// 直接当列名去查，正是这里需要的行为。
//
// inbound_id 列有意保留不删（GORM 的 sqlite AutoMigrate 本来也不删列）：
// 万一管理员回滚到旧版本二进制，旧代码读到的还是原值，行为退回单选；
// 删掉列则每条规则都会变成「所有用户」，作用域被静默放大到全体。
func migrateRoutingRuleInboundIds() error {
	if !db.Migrator().HasColumn(&model.RoutingRule{}, "inbound_id") {
		return nil
	}
	return db.Exec(`
UPDATE routing_rules
SET inbound_ids = CASE WHEN inbound_id > 0 THEN '[' || inbound_id || ']' ELSE '[]' END
WHERE inbound_ids IS NULL OR inbound_ids = ''`).Error
}

func initRouting() error {
	if err := db.AutoMigrate(&model.DomainGroup{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.OutboundNode{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.RoutingRule{}); err != nil {
		return err
	}
	return migrateRoutingRuleInboundIds()
}

func InitDB(dbPath string) error {
	dir := path.Dir(dbPath)
	err := os.MkdirAll(dir, fs.ModeDir)
	if err != nil {
		return err
	}

	var gormLogger logger.Interface

	if config.IsDebug() {
		gormLogger = logger.Default
	} else {
		gormLogger = logger.Discard
	}

	c := &gorm.Config{
		Logger: gormLogger,
	}
	db, err = gorm.Open(sqlite.Open(dbPath), c)
	if err != nil {
		return err
	}

	err = initUser()
	if err != nil {
		return err
	}
	err = initInbound()
	if err != nil {
		return err
	}
	err = initSetting()
	if err != nil {
		return err
	}
	err = initRouting()
	if err != nil {
		return err
	}

	return nil
}

func GetDB() *gorm.DB {
	return db
}

func IsNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound
}
