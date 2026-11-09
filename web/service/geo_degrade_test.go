package service

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/ipdb"
)

// regionInbound 只在内存里建一个配了地区的入站：buildGeoPlanWith 只读字段，不碰库。
func regionInbound(id int, regions string) *model.Inbound {
	return &model.Inbound{Id: id, Enable: true, Tag: "inbound-" + itoa(60000+id), Regions: regions}
}

func TestPlanUsesLastGoodWhenProvinceYieldsEmpty(t *testing.T) {
	a := regionInbound(1, `["江苏省"]`)
	tag := regionTag([]string{"江苏省"})
	lastGood := map[string][]string{tag: {"192.0.2.0/24"}}

	plan := buildGeoPlanWith([]*model.Inbound{a}, fakeProvinceDB{}, nil, lastGood)

	if len(plan.FailClosed) != 0 {
		t.Fatalf("有旧允许集可用时不该锁死入站: %v", plan.FailClosed)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Tag != tag ||
		!reflect.DeepEqual(plan.Entries[0].CIDRs, []string{"192.0.2.0/24"}) {
		t.Fatalf("应沿用旧允许集, got %#v", plan.Entries)
	}
	if plan.Degraded[a.Id].Mode != geoDegradeStale {
		t.Errorf("降级状态 = %#v, 期望 stale", plan.Degraded[a.Id])
	}
}

func TestPlanFailsClosedOnlyThatInbound(t *testing.T) {
	a := regionInbound(1, `["不存在的省"]`)
	b := regionInbound(2, `["河南省"]`)

	plan := buildGeoPlanWith([]*model.Inbound{a, b}, testProvinceDB(), nil, nil)

	if _, ok := plan.FailClosed[a.Id]; !ok || len(plan.FailClosed) != 1 {
		t.Fatalf("只有 A 应当被锁死, got %v", plan.FailClosed)
	}
	if plan.Degraded[a.Id].Mode != geoDegradeClosed {
		t.Errorf("A 的降级状态 = %#v, 期望 closed", plan.Degraded[a.Id])
	}
	if _, ok := plan.TagByInbound[b.Id]; !ok {
		t.Error("B 不该受 A 影响")
	}
	if _, ok := plan.Degraded[b.Id]; ok {
		t.Error("B 没有降级")
	}
}

func TestPlanCorruptRegionsFailsClosed(t *testing.T) {
	a := regionInbound(1, `["bad`)
	plan := buildGeoPlanWith([]*model.Inbound{a}, testProvinceDB(), nil, nil)
	if _, ok := plan.FailClosed[a.Id]; !ok {
		t.Fatal("地区数据损坏的入站应当被锁死")
	}
}

func TestPlanNilDBUsesLastGoodOrCloses(t *testing.T) {
	a := regionInbound(1, `["江苏省"]`)
	b := regionInbound(2, `["河南省"]`)
	lastGood := map[string][]string{regionTag([]string{"江苏省"}): {"192.0.2.0/24"}}

	plan := buildGeoPlanWith([]*model.Inbound{a, b}, nil, nil, lastGood)

	if plan.Degraded[a.Id].Mode != geoDegradeStale {
		t.Errorf("A = %#v, 期望 stale", plan.Degraded[a.Id])
	}
	if plan.Degraded[b.Id].Mode != geoDegradeClosed {
		t.Errorf("B = %#v, 期望 closed", plan.Degraded[b.Id])
	}
	if !strings.Contains(plan.Degraded[b.Id].Reason, "IP") {
		t.Errorf("原因 %q 没有指出 IP 库未加载", plan.Degraded[b.Id].Reason)
	}
}

func TestRulesFailClosedRule(t *testing.T) {
	a := regionInbound(1, `["不存在的省"]`)
	plan := buildGeoPlanWith([]*model.Inbound{a}, testProvinceDB(), nil, nil)
	rules := rulesFromGeoPlan([]*model.Inbound{a}, plan, "deadbeef")
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d, 期望 1 条锁死规则", len(rules))
	}
	r := rules[0].(map[string]any)
	if !reflect.DeepEqual(r["source"], []string{"0.0.0.0/0", "::/0"}) {
		t.Errorf("source = %v", r["source"])
	}
	if !strings.HasPrefix(r["ruleTag"].(string), "a-ui-geo-closed-") {
		t.Errorf("ruleTag = %v", r["ruleTag"])
	}
	if r["outboundTag"] != model.BlockOutboundTag {
		t.Errorf("outboundTag = %v", r["outboundTag"])
	}
}

func TestInjectDegradesWhenIPDBMissing(t *testing.T) {
	setupDB(t)
	cleanupGeoDat(t)
	resetGeoLastGood(t)
	in := newTestInbound(t, 60103)
	encoded, _ := EncodeRegionsStrict([]string{"江苏省"})
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", in.Id).
		UpdateColumn("regions", encoded).Error; err != nil {
		t.Fatal(err)
	}
	ipdbLock.Lock()
	saved := ipdbDBs
	ipdbDBs = map[string]*ipdb.DB{}
	ipdbLock.Unlock()
	t.Cleanup(func() {
		ipdbLock.Lock()
		ipdbDBs = saved
		ipdbLock.Unlock()
	})

	cfg := newTemplateConfig(t)
	// 一个入站的地区规则出问题不能让整份配置失败：那会冻结整机下发。
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	found := false
	for _, r := range decodeRules(t, cfg) {
		if tag, _ := r["ruleTag"].(string); strings.HasPrefix(tag, "a-ui-geo-closed-") {
			found = true
		}
	}
	if !found {
		t.Error("IP 库未加载且没有旧允许集时，该入站应当被锁死")
	}
	if GeoDegradedInbounds()[in.Id].Mode != geoDegradeClosed {
		t.Errorf("降级状态 = %#v", GeoDegradedInbounds()[in.Id])
	}
}

func resetGeoLastGood(t *testing.T) {
	t.Helper()
	geoLastGoodMu.Lock()
	geoLastGood = map[string][]string{}
	geoLastGoodSeeded = false
	geoLastGoodMu.Unlock()
}

// 写 dat 失败、但旧文件里正好有全部 tag 时，也必须标成降级：入站在用旧文件里的允许集，
// 而且写失败本身（磁盘满、无权限）需要管理员处理。改动前这里会响亮地失败。
func TestFallbackToExistingDatMarksKeptInboundsStale(t *testing.T) {
	cleanupGeoDat(t)
	tag := regionTag([]string{"江苏省"})
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: tag, CIDRs: []string{"192.0.2.0/24"}}}); err != nil {
		t.Fatal(err)
	}
	plan := &geoPlan{TagByInbound: map[int]string{1: tag}, FailClosed: map[int]string{}, Degraded: map[int]GeoDegradation{}}
	fallbackToExistingDat(plan, errors.New("disk full"))
	d := plan.Degraded[1]
	if d.Mode != geoDegradeStale || !strings.Contains(d.Reason, "disk full") {
		t.Fatalf("降级状态 = %+v，期望 stale 且带写入错误", d)
	}
	if _, closed := plan.FailClosed[1]; closed {
		t.Error("旧文件里有这个 tag，不该锁死")
	}
}
