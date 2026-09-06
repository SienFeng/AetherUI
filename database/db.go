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

// accessDB 是访问日志专用的库，与主库物理分开，原因见 model.AccessLog。
var accessDB *gorm.DB

// trafficDB 是用量历史专用的库，与主库物理分开，原因见 model.TrafficBucket。
var trafficDB *gorm.DB

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

// migrateRoutingRuleDomainGroupIds 把旧的单域名组字段 domain_group_id
// 搬到 domain_group_ids。
//
// 幂等：只回填 domain_group_ids 为空的行，面板每次启动都会跑，重启多少次
// 都安全。已有值的行绝不覆盖，否则多组规则每次重启都会被压回单组。
//
// domain_group_id <= 0 是 validate 挡不住的脏数据（直接改库可以造出来），
// 回填成 [] 后 buildRule 会因「合并后域名为空」整条丢弃并记 Warning——
// 与迁移前 domainGroupService.Get(0) 失败后跳过整条的行为完全一致。
// 迁移不改变任何一条规则的实际生效范围。
//
// domain_group_id 列有意保留不删，理由见 model.RoutingRule 的字段注释。
func migrateRoutingRuleDomainGroupIds() error {
	if !db.Migrator().HasColumn(&model.RoutingRule{}, "domain_group_id") {
		return nil
	}
	return db.Exec(`
UPDATE routing_rules
SET domain_group_ids = CASE WHEN domain_group_id > 0
                            THEN '[' || domain_group_id || ']' ELSE '[]' END
WHERE domain_group_ids IS NULL OR domain_group_ids = ''`).Error
}

func initRouting() error {
	if err := db.AutoMigrate(&model.DomainGroup{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.DomainGroupSubscription{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.OutboundNode{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.RoutingRule{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.IPBan{}); err != nil {
		return err
	}
	if err := migrateRoutingRuleInboundIds(); err != nil {
		return err
	}
	return migrateRoutingRuleDomainGroupIds()
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

// InitAccessLogDB 打开（必要时创建）访问日志库。
//
// 独立于 InitDB：即使这里失败，面板其余功能也必须照常可用——访问日志坏了
// 不该让人登不上面板。
func InitAccessLogDB(dbPath string) error {
	dir := path.Dir(dbPath)
	if err := os.MkdirAll(dir, fs.ModeDir); err != nil {
		return err
	}
	var gormLogger logger.Interface
	if config.IsDebug() {
		gormLogger = logger.Default
	} else {
		gormLogger = logger.Discard
	}
	adb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: gormLogger})
	if err != nil {
		return err
	}
	if err := adb.AutoMigrate(&model.AccessLog{}); err != nil {
		return err
	}
	accessDB = adb
	return nil
}

// InitTrafficDB 打开（必要时创建）用量历史库。
//
// 独立于 InitDB：即使这里失败，面板其余功能也必须照常可用——图表坏了不该
// 让人登不上面板，更不该影响计费用的累计流量。
func InitTrafficDB(dbPath string) error {
	dir := path.Dir(dbPath)
	if err := os.MkdirAll(dir, fs.ModeDir); err != nil {
		return err
	}
	var gormLogger logger.Interface
	if config.IsDebug() {
		gormLogger = logger.Default
	} else {
		gormLogger = logger.Discard
	}
	tdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: gormLogger})
	if err != nil {
		return err
	}
	if err := tdb.AutoMigrate(&model.TrafficBucket{}); err != nil {
		return err
	}
	// 共享检测的小时桶与用量桶同库：分库理由相同（高频写入不该抢主库
	// 那把写锁），再单开一个库只是多一个要初始化、要清理、要判空的句柄。
	if err := tdb.AutoMigrate(&model.InboundIPHour{}); err != nil {
		return err
	}
	// 域名统计与用量桶同库：分库理由相同（高频写入不该抢主库那把写锁），
	// 而且两张表的清理挂在同一个每小时任务里。
	if err := tdb.AutoMigrate(&model.DomainStat{}); err != nil {
		return err
	}
	if err := tdb.AutoMigrate(&model.DomainStatCursor{}); err != nil {
		return err
	}
	trafficDB = tdb
	return nil
}

func GetDB() *gorm.DB {
	return db
}

// GetAccessLogDB 返回访问日志库；未初始化成功时为 nil，调用方必须判空。
func GetAccessLogDB() *gorm.DB {
	return accessDB
}

// GetTrafficDB 返回用量历史库；未初始化成功时为 nil，调用方必须判空。
func GetTrafficDB() *gorm.DB {
	return trafficDB
}

func IsNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound
}

// ResetTrafficDBForTest 把用量库句柄清空，用于测试「库没打开」这条分支。
// 生产代码不调用它——面板启动时 InitTrafficDB 失败就是这个状态，而那条
// 分支上的每一个调用方都必须判空，不能靠运气。
func ResetTrafficDBForTest() {
	trafficDB = nil
}
