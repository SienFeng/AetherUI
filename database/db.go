package database

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"io/fs"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"a-ui/config"
	"a-ui/database/model"
	"a-ui/util/crypto"
)

var db *gorm.DB

// dbDirPerm 是面板自己创建库目录（/etc/<name>/）时用的权限。
//
// 0700 而不是 0750/0755：目录里有 bcrypt 哈希、会话签名密钥（settings 表的
// secret）和 IP2Location 的 token，除面板进程自己（root）之外没有任何进程需要
// 进入它。改动前传的是 fs.ModeDir——权限位为 0，建出来的是 d---------，只因
// root 无视权限位才一直没出事，换成非 root 运行就连库文件都建不出来。
// MkdirAll 只管新建的目录，已经存在的目录保持原样。
const dbDirPerm fs.FileMode = 0o700

// accessDB 是访问日志专用的库，与主库物理分开，原因见 model.AccessLog。
var accessDB atomic.Pointer[gorm.DB]

// trafficDB 是用量历史专用的库，与主库物理分开，原因见 model.TrafficBucket。
var trafficDB atomic.Pointer[gorm.DB]

// sideDBMu 串行化两个独立库的初始化；accessDBPath / trafficDBPath 记着当前句柄
// 打开的是哪个文件，只在持锁时读写。
//
// 句柄本身用 atomic.Pointer：读取方是各个定时任务与请求，与初始化方不在同一个
// goroutine 上，裸的包级变量是数据竞争。
var (
	sideDBMu      sync.Mutex
	accessDBPath  string
	trafficDBPath string
)

// DefaultUsername / DefaultPassword 是空库建出来的那个账号，也是安装向导里管理员跳过「设定账户密码」时
// 落到的账号（install.sh 的 config_after_install）。它是公开的：面板每一页顶部都会提示修改，
// 判定见 service.UserService.UsingDefaultPassword。
const (
	DefaultUsername = "admin"
	DefaultPassword = "123456"
)

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
		// 默认账号也存哈希：这一列任何时候都不该出现明文，见 util/crypto。
		hash, err := crypto.HashPassword(DefaultPassword)
		if err != nil {
			return err
		}
		user := &model.User{
			Username: DefaultUsername,
			Password: hash,
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
	if err := db.AutoMigrate(&model.InboundRegionPass{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.InboundAutoRegion{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.InboundThrottle{}); err != nil {
		return err
	}
	if err := migrateRoutingRuleInboundIds(); err != nil {
		return err
	}
	return migrateRoutingRuleDomainGroupIds()
}

func InitDB(dbPath string) error {
	dir := path.Dir(dbPath)
	err := os.MkdirAll(dir, dbDirPerm)
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
//
// 同一个文件已经打开时直接复用：面板「重启」是 SIGHUP 重建 Server，每次
// Server.Start 都会再调一遍这里。改动前每次都新开一个连接池覆盖掉旧的，旧池
// 从不关闭，点一次重启漏一个池；而换成「开新的、关旧的」也不行——旧 Server
// 还在收尾的定时任务（采集访问日志、共享检测……）在函数开头取了句柄、要用到
// 函数结束，关掉它们手里的句柄只会换来 "database is closed"。复用则谁都不受
// 影响。上次没打开成功（句柄为空）时照常重试打开。
func InitAccessLogDB(dbPath string) error {
	sideDBMu.Lock()
	defer sideDBMu.Unlock()
	if accessDB.Load() != nil && accessDBPath == dbPath {
		return nil
	}
	dir := path.Dir(dbPath)
	if err := os.MkdirAll(dir, dbDirPerm); err != nil {
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
	if err := adb.AutoMigrate(&model.AccessLog{}, &model.AccessLogCursor{}); err != nil {
		return err
	}
	accessDB.Store(adb)
	accessDBPath = dbPath
	return nil
}

// InitTrafficDB 打开（必要时创建）用量历史库。
//
// 独立于 InitDB：即使这里失败，面板其余功能也必须照常可用——图表坏了不该
// 让人登不上面板，更不该影响计费用的累计流量。
//
// 同一个文件已经打开时直接复用，理由见 InitAccessLogDB。
func InitTrafficDB(dbPath string) error {
	sideDBMu.Lock()
	defer sideDBMu.Unlock()
	if trafficDB.Load() != nil && trafficDBPath == dbPath {
		return nil
	}
	dir := path.Dir(dbPath)
	if err := os.MkdirAll(dir, dbDirPerm); err != nil {
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
	// 计量池与域名统计同库：重算时要读 DomainStat 的聚合结果，生成期要读
	// 池，两者始终一起用；孤儿清理也挂在同一个每小时任务里。
	// 共享风险快照与共享检测的小时桶同库：它是那张表的纯派生物，
	// traffic 库丢了风险分就该跟着失效。
	if err := tdb.AutoMigrate(&model.InboundRiskSnapshot{}); err != nil {
		return err
	}
	if err := tdb.AutoMigrate(&model.MeterDomain{}); err != nil {
		return err
	}
	trafficDB.Store(tdb)
	trafficDBPath = dbPath
	return nil
}

func GetDB() *gorm.DB {
	return db
}

// GetAccessLogDB 返回访问日志库；未初始化成功时为 nil，调用方必须判空。
func GetAccessLogDB() *gorm.DB {
	return accessDB.Load()
}

// GetTrafficDB 返回用量历史库；未初始化成功时为 nil，调用方必须判空。
func GetTrafficDB() *gorm.DB {
	return trafficDB.Load()
}

func IsNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound
}

// ResetTrafficDBForTest 把用量库句柄清空，用于测试「库没打开」这条分支。
// 生产代码不调用它——面板启动时 InitTrafficDB 失败就是这个状态，而那条
// 分支上的每一个调用方都必须判空，不能靠运气。
//
// 记着的路径一并清掉：测试会在「清空」之后对同一个文件再 Init 一次，模拟升级后
// 重新跑 AutoMigrate（sharing_index_test.go），那一次必须真的重新打开。
func ResetTrafficDBForTest() {
	sideDBMu.Lock()
	defer sideDBMu.Unlock()
	trafficDB.Store(nil)
	trafficDBPath = ""
}
