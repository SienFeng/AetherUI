package service

import (
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
	if plan.TagByInbound[b.Id] != regionTag([]string{"江苏省"}) {
		t.Error("B 的 tag 被连带改变了")
	}
	if plan.TagByInbound[a.Id] != regionTag([]string{"上海市", "江苏省"}) {
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
	if ended, err := s.RevokePass(auto.Id, model.RegionPassHome, now); err != nil || ended {
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
