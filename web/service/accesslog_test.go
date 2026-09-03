package service

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/accesslog"
	"a-ui/xray"
)

func TestInjectAccessLogLeavesConfigUntouchedWhenDisabled(t *testing.T) {
	cfg := &xray.Config{}
	if err := injectAccessLog(cfg, false, "bin/access.log"); err != nil {
		t.Fatalf("injectAccessLog: %v", err)
	}
	// 关闭时必须一个字节都不改：改了会让 Config.Equals 判定配置变化，
	// 那个 10 秒的重启消费任务会把所有人踢下线。
	if len(cfg.LogConfig) != 0 {
		t.Errorf("LogConfig = %s，关闭时不应注入任何内容", cfg.LogConfig)
	}
}

func TestInjectAccessLogSetsAccessPath(t *testing.T) {
	cfg := &xray.Config{}
	if err := injectAccessLog(cfg, true, "bin/access.log"); err != nil {
		t.Fatalf("injectAccessLog: %v", err)
	}
	if got := string(cfg.LogConfig); got != `{"access":"bin/access.log"}` {
		t.Errorf("LogConfig = %s，期望只设置 access", got)
	}
}

func TestInjectAccessLogPreservesOtherLogFields(t *testing.T) {
	cfg := &xray.Config{LogConfig: []byte(`{"loglevel":"debug","error":"bin/error.log"}`)}
	if err := injectAccessLog(cfg, true, "bin/access.log"); err != nil {
		t.Fatalf("injectAccessLog: %v", err)
	}
	got := string(cfg.LogConfig)
	// 模板里的 loglevel / error 是管理员自己配的，不能被顺手清掉。
	want := `{"access":"bin/access.log","error":"bin/error.log","loglevel":"debug"}`
	if got != want {
		t.Errorf("LogConfig = %s，期望 %s", got, want)
	}
}

func TestInjectAccessLogIsByteDeterministic(t *testing.T) {
	first := &xray.Config{LogConfig: []byte(`{"loglevel":"warning","error":"e.log","dnsLog":true}`)}
	second := &xray.Config{LogConfig: []byte(`{"loglevel":"warning","error":"e.log","dnsLog":true}`)}
	if err := injectAccessLog(first, true, "bin/access.log"); err != nil {
		t.Fatal(err)
	}
	if err := injectAccessLog(second, true, "bin/access.log"); err != nil {
		t.Fatal(err)
	}
	// 生成必须逐字节确定，否则 Config.Equals 恒为 false，
	// 那个 10 秒 cron 会不停重启 xray。
	if string(first.LogConfig) != string(second.LogConfig) {
		t.Errorf("两次生成结果不同:\n%s\n%s", first.LogConfig, second.LogConfig)
	}
}

func setupAccessLogDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "main.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := database.InitAccessLogDB(filepath.Join(t.TempDir(), "access.db")); err != nil {
		t.Fatalf("InitAccessLogDB: %v", err)
	}
}

func mkInbound(t *testing.T, port int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS,
		Tag:    "inbound-" + itoaS(port),
		Enable: true, Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
	}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	return in
}

func entryAt(tag, ip, target string, sec int64) accesslog.Entry {
	return accesslog.Entry{
		Time: time.Unix(sec, 0), SourceIP: ip, Network: "tcp",
		Target: target, Inbound: tag, Route: "direct", Accepted: true,
	}
}

func TestStoreResolvesInboundId(t *testing.T) {
	setupAccessLogDB(t)
	in := mkInbound(t, 50001)
	s := AccessLogService{}

	n, err := s.Store([]accesslog.Entry{
		entryAt(in.Tag, "1.2.3.4", "example.com:443", 1000),
		entryAt("inbound-59999", "5.6.7.8", "other.com:443", 1001), // 没有对应入站
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if n != 2 {
		t.Fatalf("写入 %d 条，期望 2", n)
	}

	var rows []model.AccessLog
	if err := database.GetAccessLogDB().Order("id asc").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows[0].InboundId != in.Id {
		t.Errorf("InboundId = %d，期望 %d", rows[0].InboundId, in.Id)
	}
	if rows[0].Time != time.Unix(1000, 0).UnixMilli() {
		t.Errorf("Time = %d，期望毫秒时间戳", rows[0].Time)
	}
	if rows[1].InboundId != 0 {
		t.Errorf("找不到入站时 InboundId 应为 0，实际 %d", rows[1].InboundId)
	}
	if rows[1].InboundTag != "inbound-59999" {
		t.Errorf("InboundTag = %q，原始 tag 必须留着", rows[1].InboundTag)
	}
}

func TestQueryFiltersByInboundAndPaginates(t *testing.T) {
	setupAccessLogDB(t)
	a := mkInbound(t, 50001)
	b := mkInbound(t, 50002)
	s := AccessLogService{}
	var entries []accesslog.Entry
	for i := 0; i < 5; i++ {
		entries = append(entries, entryAt(a.Tag, "1.1.1.1", "a.com:443", int64(1000+i)))
	}
	entries = append(entries, entryAt(b.Tag, "2.2.2.2", "b.com:443", 2000))
	if _, err := s.Store(entries); err != nil {
		t.Fatal(err)
	}

	list, total, err := s.Query(AccessLogQuery{InboundId: a.Id, Page: 1, PageSize: 3})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d，期望 5（只算该入站的）", total)
	}
	if len(list) != 3 {
		t.Fatalf("返回 %d 条，期望 3", len(list))
	}
	// 最新的排在最前面，管理员打开就该看到刚发生的事。
	if list[0].Time < list[1].Time {
		t.Error("结果不是按时间倒序")
	}
	page2, _, err := s.Query(AccessLogQuery{InboundId: a.Id, Page: 2, PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 {
		t.Errorf("第二页 %d 条，期望 2", len(page2))
	}
}

func TestQueryFiltersBySourceIPAndKeyword(t *testing.T) {
	setupAccessLogDB(t)
	in := mkInbound(t, 50001)
	s := AccessLogService{}
	if _, err := s.Store([]accesslog.Entry{
		entryAt(in.Tag, "1.1.1.1", "www.google.com:443", 1000),
		entryAt(in.Tag, "1.1.1.1", "www.bing.com:443", 1001),
		entryAt(in.Tag, "2.2.2.2", "www.google.com:443", 1002),
	}); err != nil {
		t.Fatal(err)
	}

	_, total, err := s.Query(AccessLogQuery{InboundId: in.Id, SourceIP: "1.1.1.1", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("按来源 IP 过滤 total = %d，期望 2", total)
	}

	_, total, err = s.Query(AccessLogQuery{InboundId: in.Id, Keyword: "google", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("按关键字过滤 total = %d，期望 2", total)
	}
}

func TestQueryKeywordTreatsWildcardsAsLiteral(t *testing.T) {
	setupAccessLogDB(t)
	in := mkInbound(t, 50001)
	s := AccessLogService{}
	if _, err := s.Store([]accesslog.Entry{
		entryAt(in.Tag, "1.1.1.1", "a.com:443", 1000),
		entryAt(in.Tag, "1.1.1.1", "b%c.com:443", 1001),
	}); err != nil {
		t.Fatal(err)
	}

	// "%" 是 LIKE 的通配符。不转义的话搜 "%" 会把所有记录都匹配上。
	_, total, err := s.Query(AccessLogQuery{InboundId: in.Id, Keyword: "%", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("搜 %% 命中 %d 条，期望只命中字面含 %% 的那 1 条", total)
	}
}

func TestCleanupDeletesOnlyExpiredRows(t *testing.T) {
	setupAccessLogDB(t)
	in := mkInbound(t, 50001)
	s := AccessLogService{}
	now := time.Unix(10_000_000, 0)
	if _, err := s.Store([]accesslog.Entry{
		entryAt(in.Tag, "1.1.1.1", "old.com:443", now.Add(-10*24*time.Hour).Unix()),
		entryAt(in.Tag, "1.1.1.1", "new.com:443", now.Add(-1*time.Hour).Unix()),
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.Cleanup(7, now)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("删除 %d 条，期望 1", deleted)
	}
	list, _, err := s.Query(AccessLogQuery{InboundId: in.Id, Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Target != "new.com:443" {
		t.Errorf("剩下的是 %+v，期望只剩 new.com:443", list)
	}
}

func TestDeleteInboundAlsoDeletesItsAccessLogs(t *testing.T) {
	setupAccessLogDB(t)
	a := mkInbound(t, 50001)
	b := mkInbound(t, 50002)
	s := AccessLogService{}
	if _, err := s.Store([]accesslog.Entry{
		entryAt(a.Tag, "1.1.1.1", "a.com:443", 1000),
		entryAt(b.Tag, "2.2.2.2", "b.com:443", 1001),
	}); err != nil {
		t.Fatal(err)
	}

	if err := (&InboundService{}).DelInbound(a.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}

	// SQLite 会复用被删除的自增 id。不连带删日志的话，下一个建出来的入站
	// 会看到上一个用户的访问记录。
	var count int64
	if err := database.GetAccessLogDB().Model(&model.AccessLog{}).
		Where("inbound_id = ?", a.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("入站已删除但还剩 %d 条访问日志", count)
	}
	if err := database.GetAccessLogDB().Model(&model.AccessLog{}).
		Where("inbound_id = ?", b.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("别人的日志被误删了，剩 %d 条", count)
	}
}

func TestAllSettingRejectsBadAccessLogSettings(t *testing.T) {
	base := validBaseSetting
	if err := base().CheckValid(); err != nil {
		t.Fatalf("基线配置应当合法: %v", err)
	}
	for _, days := range []int{0, -1, 366} {
		s := base()
		s.AccessLogRetentionDays = days
		if err := s.CheckValid(); err == nil {
			t.Errorf("保留天数 %d 应当被拒绝", days)
		}
	}
	for _, enable := range []int{-1, 2} {
		s := base()
		s.AccessLogEnable = enable
		if err := s.CheckValid(); err == nil {
			t.Errorf("启用开关取值 %d 应当被拒绝", enable)
		}
	}
}

func itoaS(i int) string {
	return strconv.Itoa(i)
}

func TestPruneOrphansRemovesLogsOfVanishedInbounds(t *testing.T) {
	setupAccessLogDB(t)
	a := mkInbound(t, 50001)
	b := mkInbound(t, 50002)
	s := AccessLogService{}
	if _, err := s.Store([]accesslog.Entry{
		entryAt(a.Tag, "1.1.1.1", "a.com:443", 1000),
		entryAt(b.Tag, "2.2.2.2", "b.com:443", 1001),
		entryAt("inbound-59999", "3.3.3.3", "c.com:443", 1002), // 写入时就没匹配上，id = 0
	}); err != nil {
		t.Fatal(err)
	}
	// 绕过 DelInbound 直接删库里的行，模拟「删除入站时日志库刚好打不开」
	// 之后留下的孤儿记录。不清掉的话，SQLite 复用 id 时会把它们挂到新用户名下。
	if err := database.GetDB().Delete(model.Inbound{}, a.Id).Error; err != nil {
		t.Fatal(err)
	}

	pruned, err := s.PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("清理 %d 条，期望 1", pruned)
	}

	var count int64
	if err := database.GetAccessLogDB().Model(&model.AccessLog{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	// b 的那条要留着；inbound_id = 0 的那条也留着——它本来就不属于任何入站，
	// 界面上查不到，交给保留期自然淘汰即可。
	if count != 2 {
		t.Errorf("剩余 %d 条，期望 2", count)
	}
}

func TestQueryPaginationIsStableWithinTheSameMillisecond(t *testing.T) {
	setupAccessLogDB(t)
	in := mkInbound(t, 50001)
	s := AccessLogService{}
	// 五条记录时间完全相同——高频访问时这非常常见。
	var entries []accesslog.Entry
	for i := 0; i < 5; i++ {
		e := entryAt(in.Tag, "1.1.1.1", "site"+strconv.Itoa(i)+".com:443", 1000)
		entries = append(entries, e)
	}
	if _, err := s.Store(entries); err != nil {
		t.Fatal(err)
	}

	// 时间相同时也必须最新的排最前：按 time 排序在这种情况下由 SQLite
	// 决定次序，实测会退化成插入顺序，最新的一条反而排到最后。
	first, _, err := s.Query(AccessLogQuery{InboundId: in.Id, Page: 1, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Target != "site4.com:443" {
		t.Errorf("第一条是 %+v，期望最后写入的 site4.com:443", first)
	}

	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		list, _, err := s.Query(AccessLogQuery{InboundId: in.Id, Page: page, PageSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range list {
			if seen[row.Target] {
				t.Errorf("第 %d 页出现了重复记录 %q——时间相同时排序不稳定会让翻页漏记录", page, row.Target)
			}
			seen[row.Target] = true
		}
	}
	// 翻完三页应当正好覆盖全部五条，一条不多一条不少。
	if len(seen) != 5 {
		t.Errorf("翻完三页只见到 %d 条，期望 5 条", len(seen))
	}
}
