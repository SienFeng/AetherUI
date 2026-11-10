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
	if len(plan.Entries) != 1 || geoTagOfCode(plan.Entries[0].Tag) != tag ||
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
	rules := rulesFromGeoPlan([]*model.Inbound{a}, plan)
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

	resetGeoGenerations(t)
	cfg := newTemplateConfig(t)
	// 一个入站的地区规则出问题不能让整份配置失败：那会冻结整机下发。
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(GeoDegradedInbounds()) != 0 {
		t.Error("核心用上之前不该显示降级（生成不等于下发）")
	}
	commitGeoApplied(cfg) // 模拟 RestartXray 下发成功
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
	// 标成已做过种子：别的测试留在磁盘上的 dat 不能混进来，否则「没有旧允许集」
	// 这个前提取决于测试执行顺序。
	geoLastGoodSeeded = true
	geoLastGoodMu.Unlock()
}

// 写 dat 失败时：旧文件里有同一组省份的旧内容，就用旧代码并标降级（内容早于这一轮，写失败本身需要管理员
// 处理）；旧文件里正好就是这份内容（同一个代码）的入站不受影响；什么都没有的锁死。改动前这里会响亮地失败。
func TestFallbackToExistingDatMarksKeptInboundsStale(t *testing.T) {
	cleanupGeoDat(t)
	tag := regionTag([]string{"江苏省"})
	oldCode := geoDatCode(tag, []string{"192.0.2.0/24"})
	same := geoDatCode(regionTag([]string{"河南省"}), []string{"198.51.100.0/24"})
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: oldCode, CIDRs: []string{"192.0.2.0/24"}},
		{Tag: same, CIDRs: []string{"198.51.100.0/24"}}}); err != nil {
		t.Fatal(err)
	}
	newCode := geoDatCode(tag, []string{"192.0.2.0/24", "203.0.113.0/24"})
	missing := geoDatCode(regionTag([]string{"上海市"}), []string{"203.0.113.0/24"})
	plan := &geoPlan{TagByInbound: map[int]string{1: newCode, 2: same, 3: missing},
		TagOfCode: map[string]string{newCode: tag, same: regionTag([]string{"河南省"}), missing: regionTag([]string{"上海市"})},
		FailClosed: map[int]string{}, Degraded: map[int]GeoDegradation{}}
	fallbackToExistingDat(plan, errors.New("disk full"))
	d := plan.Degraded[1]
	// 原因是稳定的中文说明，原始错误（"disk full"）只进日志：见 TestFallbackReasonIsStableAcrossTempPaths。
	if d.Mode != geoDegradeStale || !strings.Contains(d.Reason, "写入地区数据文件失败") || plan.TagByInbound[1] != oldCode {
		t.Fatalf("入站 1：应改用旧代码并标 stale 且说明写入失败: %+v %s", d, plan.TagByInbound[1])
	}
	if _, ok := plan.Degraded[2]; ok || plan.TagByInbound[2] != same {
		t.Errorf("入站 2：旧文件里就是这份内容，不受影响: %+v", plan.Degraded[2])
	}
	if _, closed := plan.FailClosed[3]; !closed {
		t.Error("入站 3：旧文件里没有，只能锁死")
	}
}
