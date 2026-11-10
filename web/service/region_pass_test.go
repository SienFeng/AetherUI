package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

func passOf(regions string) *model.InboundRegionPass {
	return &model.InboundRegionPass{Regions: regions}
}

func TestEffectiveRegionsUnion(t *testing.T) {
	in := regionInbound(1, `["江苏省"]`)
	got, suspended, err := effectiveRegions(in, passOf(`["上海市"]`))
	if err != nil || suspended {
		t.Fatalf("err=%v suspended=%v", err, suspended)
	}
	if !reflect.DeepEqual(got, []string{"上海市", "江苏省"}) {
		t.Errorf("有效地区 = %v", got)
	}
	if _, suspended, _ := effectiveRegions(in, passOf(`["*"]`)); !suspended {
		t.Error(`["*"] 通行证应当暂停地区限制`)
	}
	if got, _, _ := effectiveRegions(in, nil); !reflect.DeepEqual(got, []string{"江苏省"}) {
		t.Errorf("无通行证时 = %v", got)
	}
}

// 两个入站共用同一个省份集合时，一个入站发了通行证，不能连带改变另一个的 tag。
func TestPlanPassChangesOnlyThatInboundTag(t *testing.T) {
	a := regionInbound(1, `["江苏省"]`)
	b := regionInbound(2, `["江苏省"]`)
	db := fakeProvinceDB{"江苏省": {"192.0.2.0/24"}, "上海市": {"198.51.100.0/24"}}
	passes := map[int]*model.InboundRegionPass{a.Id: passOf(`["上海市"]`)}

	plan := buildGeoPlanWith([]*model.Inbound{a, b}, db, passes, nil)

	if plan.TagByInbound[a.Id] == plan.TagByInbound[b.Id] {
		t.Fatal("A 有通行证，tag 应当与 B 不同")
	}
	if geoTagOfCode(plan.TagByInbound[b.Id]) != regionTag([]string{"江苏省"}) {
		t.Error("B 的 tag 被连带改变了")
	}
	if geoTagOfCode(plan.TagByInbound[a.Id]) != regionTag([]string{"上海市", "江苏省"}) {
		t.Error("A 的 tag 应当按有效地区计算")
	}
}

func TestPlanSuspendedPassSkipsAndToleratesNilDB(t *testing.T) {
	a := regionInbound(1, `["江苏省"]`)
	passes := map[int]*model.InboundRegionPass{a.Id: passOf(`["*"]`)}
	plan := buildGeoPlanWith([]*model.Inbound{a}, nil, passes, nil)
	if len(plan.TagByInbound) != 0 || len(plan.FailClosed) != 0 || len(plan.Entries) != 0 {
		t.Fatalf("暂停地区限制的入站不该生成任何东西: %+v", plan)
	}
}

func TestGeoBlockMatcherSkipsSuspended(t *testing.T) {
	a := regionInbound(1, `["江苏省"]`)
	stored := func([]string) (*cidrSet, error) { return newCIDRSet([]string{"192.0.2.0/24"}), nil }
	g := newGeoBlockMatcherWith([]*model.Inbound{a}, map[int]*model.InboundRegionPass{a.Id: passOf(`["*"]`)}, stored)
	if g.Blocks(a.Id, "192.0.2.1") {
		t.Error("暂停地区限制期间，存储地区之内的来源不该算作地区拦截")
	}
}

func setupPassInbound(t *testing.T, port int, regions []string) *model.Inbound {
	t.Helper()
	setupGeoDB(t)
	requireIPDB(t)
	return mkRegionInbound(t, port, true, regions)
}

func TestGrantReplacesActive(t *testing.T) {
	in := setupPassInbound(t, 60301, []string{"江苏省"})
	s := RegionPassService{}
	now := time.Unix(1_800_000_000, 0)
	first, err := s.Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), false, now)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	second, err := s.Grant(in.Id, []string{"*"}, now.Add(2*time.Hour).UnixMilli(), false, now)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	active, err := s.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[in.Id].Id != second.Id {
		t.Fatalf("活动通行证 = %+v，期望只有第二张", active)
	}
	var old model.InboundRegionPass
	database.GetDB().First(&old, first.Id)
	if old.EndReason != model.RegionPassReplaced || old.EndedAt == 0 {
		t.Errorf("第一张应以 replaced 结束: %+v", old)
	}
}

func TestGrantRejectsUnknownProvince(t *testing.T) {
	in := setupPassInbound(t, 60302, []string{"江苏省"})
	now := time.Unix(1_800_000_000, 0)
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"火星省"}, now.Add(time.Hour).UnixMilli(), false, now); err == nil {
		t.Error("不存在的省份应当被拒")
	}
}

func TestGrantRejectsInboundWithoutRegions(t *testing.T) {
	in := setupPassInbound(t, 60303, nil)
	now := time.Unix(1_800_000_000, 0)
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), false, now); err == nil {
		t.Error("没配地区限制的入站不需要通行证，应当被拒")
	}
}

func TestExpireDue(t *testing.T) {
	in := setupPassInbound(t, 60304, []string{"江苏省"})
	s := RegionPassService{}
	now := time.Unix(1_800_000_000, 0)
	p, err := s.Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), false, now)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireDue(now.Add(30 * time.Minute)); err != nil || n != 0 {
		t.Fatalf("未到期: n=%d err=%v", n, err)
	}
	if n, err := s.ExpireDue(now.Add(2 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("到期: n=%d err=%v", n, err)
	}
	var got model.InboundRegionPass
	database.GetDB().First(&got, p.Id)
	if got.EndReason != model.RegionPassExpired {
		t.Errorf("EndReason = %q", got.EndReason)
	}
	if n, _ := s.ExpireDue(now.Add(3 * time.Hour)); n != 0 {
		t.Errorf("重复清理返回 %d", n)
	}
}

// 通行证不在 Inbound 表上：编辑入站（改备注）不能碰到它。
func TestUpdateInboundKeepsPass(t *testing.T) {
	in := setupPassInbound(t, 60305, []string{"江苏省"})
	now := time.Now()
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), false, now); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, in.Id)
	got.Remark = "改个备注"
	if err := (&InboundService{}).UpdateInbound(got); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}
	if p, err := (&RegionPassService{}).ActiveOf(in.Id); err != nil || p == nil {
		t.Fatalf("编辑入站后通行证没了: %v", err)
	}
}

func TestDelInboundDeletesPasses(t *testing.T) {
	in := setupPassInbound(t, 60306, []string{"江苏省"})
	now := time.Now()
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), false, now); err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}
	var n int64
	database.GetDB().Model(&model.InboundRegionPass{}).Where("inbound_id = ?", in.Id).Count(&n)
	if n != 0 {
		t.Errorf("删除入站后还剩 %d 张通行证（SQLite 会复用 id）", n)
	}
}

// 发了通行证之后，发证之前那些被地区规则拦下的日志行仍然是「地区拦截」：按通行证生效后的
// 有效地区回放，它们会被算成「走了其他出站」，覆盖这个用户的每条分流规则都会变红。
func TestGeoBlockMatcherKeepsStoredRegionBlocksDuringPass(t *testing.T) {
	a := regionInbound(1, `["江苏省"]`)
	stored := func([]string) (*cidrSet, error) { return newCIDRSet([]string{"192.0.2.0/24"}), nil }
	for _, regions := range []string{`["*"]`, `["北京市"]`} {
		g := newGeoBlockMatcherWith([]*model.Inbound{a}, map[int]*model.InboundRegionPass{a.Id: passOf(regions)}, stored)
		if !g.Blocks(a.Id, "198.51.100.7") {
			t.Errorf("通行证 %s 期间，存储地区之外的来源仍应算作地区拦截", regions)
		}
		if g.Blocks(a.Id, "192.0.2.9") {
			t.Errorf("通行证 %s 期间，存储地区之内的来源不该算作地区拦截", regions)
		}
	}
}

// 自动放行只结束它自己判定时看到的那张通行证：管理员这期间新发的手动通行证不能被连带收回。
func TestRevokePassOnlyEndsThatPass(t *testing.T) {
	in := setupPassInbound(t, 60307, []string{"江苏省"})
	s := RegionPassService{}
	now := time.Now()
	auto, err := s.Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), true, now)
	if err != nil {
		t.Fatal(err)
	}
	manual, err := s.Grant(in.Id, []string{"*"}, now.Add(time.Hour).UnixMilli(), false, now)
	if err != nil {
		t.Fatal(err)
	}
	if ended, err := s.RevokePass(in.Id, auto.Id, model.RegionPassHome, now); err != nil || ended {
		t.Fatalf("已被替换的自动通行证不该再结束任何东西: ended=%v err=%v", ended, err)
	}
	if p, _ := s.ActiveOf(in.Id); p == nil || p.Id != manual.Id {
		t.Fatalf("手动通行证被连带收回了: %+v", p)
	}
}

// 自动发放要「比较后设置」：判定之后管理员发了手动通行证，自动发放必须放弃而不是把它替换掉。
func TestGrantAutoRefusesWhenPassAppeared(t *testing.T) {
	in := setupPassInbound(t, 60308, []string{"江苏省"})
	s := RegionPassService{}
	now := time.Now()
	manual, err := s.Grant(in.Id, []string{"*"}, now.Add(time.Hour).UnixMilli(), false, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GrantAuto(in.Id, "上海市", now.Add(time.Hour).UnixMilli(), now); err == nil {
		t.Fatal("已有通行证时自动发放应当放弃")
	}
	if p, _ := s.ActiveOf(in.Id); p == nil || p.Id != manual.Id {
		t.Fatalf("手动通行证被自动发放替换了: %+v", p)
	}
}

// 续期要说实话：通行证已经结束（被收回、被替换、到期）时不能报「已续期」。按 id 收回与续期
// 还要对得上入站：SQLite 会复用 id，只认 id 可能碰到别的入站的那一张。
func TestRenewAndRevokePassMatchTheInboundAndReportTruth(t *testing.T) {
	in := setupPassInbound(t, 60308, []string{"江苏省"})
	other := mkRegionInbound(t, 60309, true, []string{"江苏省"})
	s := RegionPassService{}
	now := time.Now()
	p, err := s.Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), true, now)
	if err != nil {
		t.Fatal(err)
	}
	if ended, err := s.RevokePass(other.Id, p.Id, model.RegionPassHome, now); err != nil || ended {
		t.Fatalf("入站对不上时不该收回: ended=%v err=%v", ended, err)
	}
	if renewed, err := s.Renew(other.Id, p.Id, now.Add(2*time.Hour).UnixMilli()); err != nil || renewed {
		t.Fatalf("入站对不上时不该续期: renewed=%v err=%v", renewed, err)
	}
	if ended, err := s.RevokePass(in.Id, p.Id, model.RegionPassHome, now); err != nil || !ended {
		t.Fatalf("应当收回: ended=%v err=%v", ended, err)
	}
	if renewed, err := s.Renew(in.Id, p.Id, now.Add(2*time.Hour).UnixMilli()); err != nil || renewed {
		t.Fatalf("已结束的通行证不该报续期成功: renewed=%v err=%v", renewed, err)
	}
}

// 关掉入站的「自动出差放行」：生效中的自动通行证一并结束（之后没人管它的收回与续期），
// 管理员手动发的不动。
func TestUpdateInboundTurningAutoTravelOffEndsAutoPass(t *testing.T) {
	in := setupPassInbound(t, 60310, []string{"江苏省"})
	db := database.GetDB()
	if err := db.Model(&model.Inbound{}).Where("id = ?", in.Id).UpdateColumn("auto_travel", true).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := (&RegionPassService{}).GrantAuto(in.Id, "上海市", now.Add(time.Hour).UnixMilli(), now); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, in.Id)
	got.AutoTravel = false
	if err := (&InboundService{}).UpdateInbound(got); err != nil {
		t.Fatal(err)
	}
	if p, _ := (&RegionPassService{}).ActiveOf(in.Id); p != nil {
		t.Errorf("关掉自动出差放行后自动通行证仍在生效: %+v", p)
	}

	manual := mkRegionInbound(t, 60311, true, []string{"江苏省"})
	if err := db.Model(&model.Inbound{}).Where("id = ?", manual.Id).UpdateColumn("auto_travel", true).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&RegionPassService{}).Grant(manual.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), false, now); err != nil {
		t.Fatal(err)
	}
	m := mustGet(t, manual.Id)
	m.AutoTravel = false
	if err := (&InboundService{}).UpdateInbound(m); err != nil {
		t.Fatal(err)
	}
	if p, _ := (&RegionPassService{}).ActiveOf(manual.Id); p == nil {
		t.Error("管理员手动发的通行证不该跟着结束")
	}
}

// 预演只针对一个存在的入站：id 为 0 在 windowRows 里表示「所有入站」，会把所有客户的来源混在一起。
func TestPreviewRejectsMissingInbound(t *testing.T) {
	setupPassInbound(t, 60312, nil)
	for _, id := range []int{0, -1, 999999} {
		if _, err := (&RegionPreviewService{}).Preview(id, []string{"江苏省"}, time.Now()); err == nil {
			t.Errorf("入站 %d 不存在，预演应当报错", id)
		}
	}
}

// 「需要留意」给被拦来源标的省份要能真的放行它：多数票的省份在执行口径下不一定包含这个 IP
// （按多数判据合并时各库平票），那时一键发的通行证放行不了它，宁可标成未知。
func TestEnforceableProvince(t *testing.T) {
	calls := 0
	allowed := func(regions []string) (*cidrSet, error) {
		calls++
		return newCIDRSet([]string{"192.0.2.0/24"}), nil
	}
	cache := map[string]*cidrSet{}
	if got := enforceableProvince("192.0.2.5", "江苏省", allowed, cache); got != "江苏省" {
		t.Errorf("在允许集里的 IP 应保留省份: %q", got)
	}
	if got := enforceableProvince("198.51.100.5", "江苏省", allowed, cache); got != "" {
		t.Errorf("放行该省也放不进来的 IP 应标成未知: %q", got)
	}
	if got := enforceableProvince("198.51.100.5", "", allowed, cache); got != "" {
		t.Errorf("没有省份的保持为空: %q", got)
	}
	if calls != 1 {
		t.Errorf("同一个省的允许集只建一次，实际 %d 次", calls)
	}
}

// 并发判定（一个人一个网络）跳过「被核心挡在门外」的来源，那个判断必须只看核心此刻生效的地区规则。
// 回放 24 小时日志用的匹配器对持有通行证的入站把「存储地区之外」一律算成被拦（storedAllow）；
// 并发判定若复用它，通行证有效期间通行证省份里的网络全部不占名额——五个人在外省同时用也一个都不断。
func TestConcurrencySourceBlockerIgnoresStoredRegionReplay(t *testing.T) {
	in := setupPassInbound(t, 60331, []string{"江苏省"})
	isolateLiveGeo(t)
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"*"}, time.Now().Add(time.Hour).UnixMilli(), false, time.Now()); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	concurrencyBlockMu.Lock()
	concurrencyBlockGeo = nil
	concurrencyBlockMu.Unlock()
	t.Cleanup(func() {
		concurrencyBlockMu.Lock()
		concurrencyBlockGeo = nil
		concurrencyBlockMu.Unlock()
	})
	blocked := (&ConcurrencyService{}).sourceBlocker([]*model.Inbound{in}, time.Now())(in)
	if blocked("198.51.100.1") {
		t.Error("地区限制已暂停（* 通行证），核心不拦任何来源，并发判定却把江苏以外的来源当成被拦、不计名额")
	}
}

// 单省通行证（存储江苏、通行证上海）：两个网络同时在用，额度 1 时要断掉后到的那个。改动前存储地区之外的
// 来源一律被当成「被核心拦着」，两个都不占名额，一个都不断。测试环境里核心还没用上过地区规则、dat 里也
// 没有这一组，所以这里只看「回放分支不再参与」；真实核心对允许集之外的来源照常拦截、照常不占名额。
func TestConcurrencyCountsNetworksDuringSingleProvincePass(t *testing.T) {
	in := setupPassInbound(t, 60332, []string{"江苏省"})
	isolateLiveGeo(t)
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"上海市"}, time.Now().Add(time.Hour).UnixMilli(), true, time.Now()); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	concurrencyBlockMu.Lock()
	concurrencyBlockGeo = nil
	concurrencyBlockMu.Unlock()
	t.Cleanup(func() {
		concurrencyBlockMu.Lock()
		concurrencyBlockGeo = nil
		concurrencyBlockMu.Unlock()
	})
	blocked := (&ConcurrencyService{}).sourceBlocker([]*model.Inbound{in}, time.Now())(in)
	list := []OnlineIP{
		{IP: "192.0.2.21", Conns: 1, FirstSeen: 100},
		{IP: "198.51.100.21", Conns: 1, FirstSeen: 200},
	}
	if got := planNetworkRejections(list, 1, blocked, nil); !reflect.DeepEqual(got, []string{"198.51.100.21"}) {
		t.Errorf("通行证期间两个网络同时在用、额度 1，应当断开后到的那个，实际 %v", got)
	}
}

// 接到判定入口：存储江苏、通行证上海。上海的来源先连上、一直连着，本人后回到江苏的家里、已经在用——
// 额度 1 时断开上海的那个，而不是本人。改动前按先来后到断开本人，直到通行证被收回。
func TestConcurrencyPrefersHomeNetworkDuringPass(t *testing.T) {
	in := setupPassInbound(t, 60333, []string{"江苏省"})
	isolateLiveGeo(t)
	orig := concurrencyHomeSetOf
	concurrencyHomeSetOf = func([]string) (*cidrSet, error) { return newCIDRSet([]string{"192.0.2.0/24"}), nil }
	t.Cleanup(func() { concurrencyHomeSetOf = orig })
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"上海市"}, time.Now().Add(time.Hour).UnixMilli(), true, time.Now()); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	concurrencyBlockMu.Lock()
	concurrencyBlockGeo = nil
	concurrencyBlockMu.Unlock()
	t.Cleanup(func() {
		concurrencyBlockMu.Lock()
		concurrencyBlockGeo, concurrencyHome = nil, nil
		concurrencyBlockMu.Unlock()
	})
	s := &ConcurrencyService{}
	blocked := s.sourceBlocker([]*model.Inbound{in}, time.Now())(in)
	list := []OnlineIP{
		{IP: "198.51.100.21", Conns: 1, FirstSeen: 100, Down: coexistMinActiveBytes},
		{IP: "192.0.2.21", Conns: 1, FirstSeen: 200, Down: coexistMinActiveBytes},
	}
	if got := planNetworkRejections(list, 1, blocked, s.homeFor(in)); !reflect.DeepEqual(got, []string{"198.51.100.21"}) {
		t.Errorf("通行证期间本人回到家里省，应当断开通行证省份那边，实际 %v", got)
	}
}

// isolateLiveGeo 让并发判定的匹配器只看「核心还没用上任何地区规则、dat 里也没有」的状态，不受同一进程里
// 其它用例留下的已生效地区与 dat 文件影响（那时允许集之外的来源会被真实规则拦下，测不到回放分支）。
func isolateLiveGeo(t *testing.T) {
	t.Helper()
	resetGeoGenerations(t)
	if _, err := os.Stat(geoDatPath); err == nil {
		aside := filepath.Join(t.TempDir(), "a-ui-geo.dat")
		if err := os.Rename(geoDatPath, aside); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Rename(aside, geoDatPath) })
	}
}
