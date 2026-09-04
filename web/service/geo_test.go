package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/ipdb"
)

func TestDecodeRegionsNormalizes(t *testing.T) {
	cases := []struct {
		name    string
		encoded string
		want    []string
	}{
		{"空字符串", "", nil},
		{"null", "null", nil},
		{"空数组", "[]", nil},
		// 排序 + 去重：省份集合决定 dat 里的 tag，顺序不定就会为同一个集合
		// 生成两个 tag，dat 里出现重复数据。
		{"排序去重", `["河南省","江苏省","河南省"]`, []string{"江苏省", "河南省"}},
		{"去掉空白项", `["江苏省","  ",""]`, []string{"江苏省"}},
		{"两端空白", `[" 江苏省 "]`, []string{"江苏省"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DecodeRegions(c.encoded)
			if err != nil {
				t.Fatalf("DecodeRegions: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("得到 %v，期望 %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("得到 %v，期望 %v", got, c.want)
				}
			}
		})
	}
}

func TestDecodeRegionsRejectsCorruptData(t *testing.T) {
	if _, err := DecodeRegions(`{"not":"an array"}`); err == nil {
		t.Error("数据损坏时必须报错，返回空列表会让地区限制静默失效")
	}
}

func TestEncodeRegionsStrictRejectsSilentEmptying(t *testing.T) {
	// 与入站选择同一类事故：用户选了地区，全被过滤成空，结果变成"不限制"，
	// 界面上却显示他选过。
	if _, err := EncodeRegionsStrict([]string{" ", ""}); err == nil {
		t.Error("非空输入被过滤成空时必须报错")
	}
	encoded, err := EncodeRegionsStrict(nil)
	if err != nil {
		t.Fatalf("空输入应当合法（表示不限制）: %v", err)
	}
	if encoded != "[]" {
		t.Errorf("空输入编码为 %q，期望 []", encoded)
	}
}

func TestRegionTagIsStableAndSetBased(t *testing.T) {
	a := regionTag([]string{"江苏省", "河南省"})
	b := regionTag([]string{"江苏省", "河南省"})
	if a != b {
		t.Errorf("同一组省份生成了不同的 tag: %q / %q", a, b)
	}
	if c := regionTag([]string{"江苏省"}); c == a {
		t.Error("不同省份集合生成了相同的 tag")
	}
	// tag 会写进 dat 也会写进配置字符串，必须是纯 ASCII 大写，
	// 避免大小写不敏感匹配与中文编码带来的意外。
	if a != strings.ToUpper(a) {
		t.Errorf("tag %q 不是全大写", a)
	}
	for _, r := range a {
		if !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			t.Fatalf("tag %q 含非 ASCII 字母数字字符 %q", a, r)
		}
	}
}

func setupGeoDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "geo.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func mkRegionInbound(t *testing.T, port int, enable bool, regions []string) *model.Inbound {
	t.Helper()
	encoded, err := EncodeRegionsStrict(regions)
	if err != nil {
		t.Fatalf("EncodeRegionsStrict: %v", err)
	}
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS,
		Tag: "inbound-" + itoaS(port), Enable: enable,
		Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
		Regions: encoded,
	}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	return in
}

// 只用到 CIDR 查询，测试里用一个固定的假实现，避免依赖 2 MB 的真实库。
type fakeProvinceDB map[string][]string

func (f fakeProvinceDB) CIDRsOfProvinces(provinces []string) []string {
	var out []string
	for _, p := range provinces {
		out = append(out, f[p]...)
	}
	return out
}

func testProvinceDB() fakeProvinceDB {
	return fakeProvinceDB{
		"江苏省": {"58.192.0.0/11", "114.212.0.0/14"},
		"河南省": {"1.24.0.0/13"},
	}
}

func TestBuildGeoEntriesDedupesByProvinceSet(t *testing.T) {
	setupGeoDB(t)
	a := mkRegionInbound(t, 60001, true, []string{"江苏省"})
	b := mkRegionInbound(t, 60002, true, []string{"江苏省"})
	c := mkRegionInbound(t, 60003, true, []string{"江苏省", "河南省"})
	mkRegionInbound(t, 60004, true, nil)              // 不限制
	mkRegionInbound(t, 60005, false, []string{"江苏省"}) // 已停用

	plan, err := buildGeoPlan([]*model.Inbound{
		mustGet(t, a.Id), mustGet(t, b.Id), mustGet(t, c.Id),
		mustGet(t, a.Id+3), mustGet(t, a.Id+4),
	}, testProvinceDB())
	if err != nil {
		t.Fatalf("buildGeoPlan: %v", err)
	}

	// 两个入站用同一组省份，dat 里只该出现一份数据。
	if len(plan.Entries) != 2 {
		t.Fatalf("dat 里有 %d 组，期望 2 组（江苏 / 江苏+河南）", len(plan.Entries))
	}
	if plan.TagByInbound[a.Id] != plan.TagByInbound[b.Id] {
		t.Error("相同省份的两个入站应当共用同一个 tag")
	}
	if plan.TagByInbound[c.Id] == plan.TagByInbound[a.Id] {
		t.Error("不同省份集合不该共用 tag")
	}
	if _, ok := plan.TagByInbound[a.Id+3]; ok {
		t.Error("没设地区的入站不该出现在计划里")
	}
	if _, ok := plan.TagByInbound[a.Id+4]; ok {
		t.Error("已停用的入站不该出现在计划里")
	}
	// entry 顺序必须确定，否则 dat 内容哈希每轮都变，xray 会被反复重启。
	if plan.Entries[0].Tag > plan.Entries[1].Tag {
		t.Error("entry 没有按 tag 升序排列")
	}
}

func mustGet(t *testing.T, id int) *model.Inbound {
	t.Helper()
	in, err := (&InboundService{}).GetInbound(id)
	if err != nil {
		t.Fatalf("GetInbound(%d): %v", id, err)
	}
	return in
}

func TestBuildGeoPlanRejectsProvinceWithNoCIDRs(t *testing.T) {
	setupGeoDB(t)
	in := mkRegionInbound(t, 60001, true, []string{"不存在的省"})

	_, err := buildGeoPlan([]*model.Inbound{mustGet(t, in.Id)}, testProvinceDB())
	// 允许集为空会让「来源不在集合里」匹配到所有人，该入站被彻底封死。
	// 这种情况必须报错让整份配置生成失败，而不是生成一条把人全挡住的规则。
	if err == nil {
		t.Fatal("查不到任何 IP 段时必须报错")
	}
	if !strings.Contains(err.Error(), "不存在的省") {
		t.Errorf("错误信息 %q 里没有指出是哪个地区", err)
	}
}

func TestBuildGeoRulesEmitsIPv6DenyBeforeAllowSet(t *testing.T) {
	setupGeoDB(t)
	in := mkRegionInbound(t, 60001, true, []string{"江苏省"})

	rules, err := buildGeoRules([]*model.Inbound{mustGet(t, in.Id)}, testProvinceDB(), "deadbeef")
	if err != nil {
		t.Fatalf("buildGeoRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("生成了 %d 条规则，期望 2 条（IPv6 拒绝 + 允许集取反）", len(rules))
	}

	first := rules[0].(map[string]any)
	second := rules[1].(map[string]any)

	// 实测确认：纯 IPv4 的允许集配上 ! 取反，遇到 IPv6 来源会 fail open。
	// 必须另有一条 ::/0 的拒绝规则，且排在前面。
	if src := first["source"].([]string); len(src) != 1 || src[0] != "::/0" {
		t.Errorf("第一条规则的 source = %v，期望 [::/0]", first["source"])
	}
	if first["outboundTag"] != model.BlockOutboundTag {
		t.Errorf("IPv6 规则没有指向黑洞出站")
	}

	src := second["source"].([]string)
	if len(src) != 1 || !strings.HasPrefix(src[0], "ext:") || !strings.Contains(src[0], ":!") {
		t.Errorf("允许集规则的 source = %v，期望 ext:<文件>:!<TAG> 形式", src)
	}
	// dat 内容变化不体现在配置字节里，Config.Equals 察觉不到。
	// 把内容哈希嵌进 ruleTag，哈希变则配置字节变，重启逻辑天然正确。
	tag, _ := second["ruleTag"].(string)
	if !strings.Contains(tag, "deadbeef") {
		t.Errorf("ruleTag = %q，里面没有 dat 内容哈希", tag)
	}

	for i, r := range []map[string]any{first, second} {
		tags, ok := r["inboundTag"].([]string)
		if !ok || len(tags) != 1 || tags[0] != in.Tag {
			t.Errorf("第 %d 条规则的 inboundTag = %v，期望 [%s]", i, r["inboundTag"], in.Tag)
		}
	}
}

func TestBuildGeoRulesEmitsNothingWithoutRegions(t *testing.T) {
	setupGeoDB(t)
	in := mkRegionInbound(t, 60001, true, nil)

	rules, err := buildGeoRules([]*model.Inbound{mustGet(t, in.Id)}, testProvinceDB(), "deadbeef")
	if err != nil {
		t.Fatalf("buildGeoRules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("没配地区却生成了 %d 条规则", len(rules))
	}
}

func TestGeoRulesAreByteDeterministic(t *testing.T) {
	setupGeoDB(t)
	a := mkRegionInbound(t, 60001, true, []string{"河南省", "江苏省"})
	b := mkRegionInbound(t, 60002, true, []string{"江苏省"})
	list := []*model.Inbound{mustGet(t, a.Id), mustGet(t, b.Id)}

	first, err := buildGeoRules(list, testProvinceDB(), "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	encodedFirst, _ := json.Marshal(first)
	for i := 0; i < 5; i++ {
		again, err := buildGeoRules(list, testProvinceDB(), "abcd1234")
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(again)
		if string(encoded) != string(encodedFirst) {
			t.Fatalf("第 %d 次生成结果不同:\n%s\n%s", i, encodedFirst, encoded)
		}
	}
}

func TestWriteGeoDatOnlyRewritesWhenContentChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a-ui-geo.dat")
	entries := geoEntriesFor(t, map[string][]string{"A": {"1.0.0.0/8"}})

	hash1, err := writeGeoDat(path, entries)
	if err != nil {
		t.Fatalf("writeGeoDat: %v", err)
	}
	st1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	hash2, err := writeGeoDat(path, entries)
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Errorf("相同内容算出了不同的哈希: %q / %q", hash1, hash2)
	}
	st2, _ := os.Stat(path)
	// 内容没变就不该重写：这个函数每 10 秒被走到一次。
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("内容未变化却重写了文件")
	}

	changed := geoEntriesFor(t, map[string][]string{"A": {"2.0.0.0/8"}})
	hash3, err := writeGeoDat(path, changed)
	if err != nil {
		t.Fatal(err)
	}
	if hash3 == hash1 {
		t.Error("内容变了哈希却没变，xray 不会重新加载 dat")
	}
}

func TestWriteGeoDatRecreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a-ui-geo.dat")
	entries := geoEntriesFor(t, map[string][]string{"A": {"1.0.0.0/8"}})

	if _, err := writeGeoDat(path, entries); err != nil {
		t.Fatal(err)
	}
	// 有人手工删了文件。只按哈希判断的话会以为无需重写，
	// xray 下次启动就会因为找不到 dat 而失败——全员断网。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := writeGeoDat(path, entries); err != nil {
		t.Fatalf("writeGeoDat: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("文件没有被重建: %v", err)
	}
}

func geoEntriesFor(t *testing.T, m map[string][]string) []geodatEntry {
	t.Helper()
	var out []geodatEntry
	for tag, cidrs := range m {
		out = append(out, geodatEntry{Tag: tag, CIDRs: cidrs})
	}
	return out
}

// requireIPDB 加载仓库里那份真实的归属地库。注入路径必须用真库测：
// 假的 CIDR 源测不出「省份名对不上」「库没加载」这类真实故障。
//
// 这里显式指向仓库里的 bin/ipdb.dat，而不是生产的落盘位置
// （/etc/<name>/ipdb.dat）：后者在开发机上不存在，直接用会让下面这批
// 真实数据用例整体静默跳过，等于把覆盖悄悄删掉。
func requireIPDB(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(legacyIP2RegionPath); err != nil {
		t.Skipf("IP 归属地库不存在: %v", err)
	}
	useTestSources(t, []ipdbSource{{
		Key: ip2regionKey, Name: "ip2region", Path: legacyIP2RegionPath,
		MinSegments: minValidSegments,
		URL:         func(*SettingService) (string, error) { return "", nil },
		Build:       ipdb.Build,
	}})
	if err := (&IPDBService{}).Load(); err != nil {
		t.Fatalf("加载 IP 库: %v", err)
	}
}

// cleanupGeoDat 删掉测试写出的 dat。生产路径就是 bin/a-ui-geo.dat，
// 这里不做路径替换，为的是把「真的能写到那个位置」也一起验了。
func cleanupGeoDat(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = os.Remove(geoDatPath) })
}

func TestInjectPutsGeoRulesBeforeBlockAndProxyRules(t *testing.T) {
	setupDB(t)
	requireIPDB(t)
	cleanupGeoDat(t)

	in := newTestInbound(t, 60101)
	encoded, err := EncodeRegionsStrict([]string{"江苏省"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", in.Id).
		UpdateColumn("regions", encoded).Error; err != nil {
		t.Fatal(err)
	}

	banned := newTestGroup(t, "违规域名")
	node, _ := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	chatgpt := newTestGroup(t, "ChatGPT")
	rs := RoutingRuleService{}
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: chatgpt.Id, Action: model.ActionProxy, OutboundId: node.Id,
		Priority: 1, Enable: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: banned.Id, Action: model.ActionBlock, Priority: 99, Enable: true,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)

	// 期望顺序：模板原有规则 → 地区规则(2 条) → block 规则 → proxy 规则。
	// 地区限制是准入判定，必须先于任何分流决策——排在分流之后的话，
	// 非江苏用户访问被分流的域名会先命中分流规则走代理出站，限制被静默绕过。
	if len(rules) != 5 {
		t.Fatalf("规则数 = %d，期望 5：%v", len(rules), rules)
	}
	if tag, _ := rules[0]["inboundTag"].([]any); len(tag) != 1 || tag[0] != "api" {
		t.Errorf("rules[0] 不是模板原有规则: %v", rules[0])
	}
	if src, _ := rules[1]["source"].([]any); len(src) != 1 || src[0] != "::/0" {
		t.Errorf("rules[1] 应当是 IPv6 拒绝规则: %v", rules[1])
	}
	if src, _ := rules[2]["source"].([]any); len(src) != 1 ||
		!strings.HasPrefix(src[0].(string), "ext:a-ui-geo.dat:!") {
		t.Errorf("rules[2] 应当是允许集取反规则: %v", rules[2])
	}
	if rules[3]["outboundTag"] != model.BlockOutboundTag {
		t.Errorf("rules[3] 应当是 block 规则: %v", rules[3])
	}
	if rules[4]["outboundTag"] != node.Tag {
		t.Errorf("rules[4] 应当是 proxy 规则: %v", rules[4])
	}

	// dat 必须真的落到生产路径上，否则 xray 启动时找不到文件会直接失败。
	st, err := os.Stat(geoDatPath)
	if err != nil {
		t.Fatalf("dat 没有生成: %v", err)
	}
	if st.Size() == 0 {
		t.Error("dat 是空文件")
	}
}

func TestInjectSkipsGeoEntirelyWhenNoInboundUsesRegions(t *testing.T) {
	setupDB(t)
	cleanupGeoDat(t)
	newTestInbound(t, 60102)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// 没人用地区限制时不碰 IP 库、也不生成 dat：这条路径要能在没有
	// 归属地库的机器上照常工作。
	if _, err := os.Stat(geoDatPath); !os.IsNotExist(err) {
		t.Errorf("没人配地区却生成了 dat 文件 (err=%v)", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 1 {
		t.Errorf("规则数 = %d，期望只有模板原有的 1 条", len(rules))
	}
}

func TestInjectFailsLoudlyWhenIPDBMissingButRegionsConfigured(t *testing.T) {
	setupDB(t)
	cleanupGeoDat(t)
	in := newTestInbound(t, 60103)
	encoded, _ := EncodeRegionsStrict([]string{"江苏省"})
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", in.Id).
		UpdateColumn("regions", encoded).Error; err != nil {
		t.Fatal(err)
	}
	// 直接把包级变量清空，模拟「库文件缺失/损坏，Load 失败」的部署。
	// 测试与实现同包，不需要为此在生产代码里开测试专用的口子。
	ipdbLock.Lock()
	ipdbDBs = map[string]*ipdb.DB{}
	ipdbLock.Unlock()

	cfg := newTemplateConfig(t)
	err := (&RoutingInjector{}).Inject(cfg)
	// 绝不能因为 IP 库没加载就把地区规则悄悄省掉——那等于地区限制失效，
	// 而面板上一切显示正常。宁可让整份配置生成失败、xray 保持原状。
	if err == nil {
		t.Fatal("IP 库未加载却生成了配置，地区限制会静默失效")
	}
	if !strings.Contains(err.Error(), "IP") {
		t.Errorf("错误信息 %q 没有指出是 IP 库的问题", err)
	}
}

func TestAddAndUpdateInboundNormalizeRegions(t *testing.T) {
	setupGeoDB(t)
	s := InboundService{}
	in := &model.Inbound{
		UserId: 1, Port: 60201, Protocol: model.VLESS, Tag: "inbound-60201",
		Enable: true, Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
		Regions: `["河南省","江苏省","河南省"]`,
	}
	if err := s.AddInbound(in); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	got := mustGet(t, in.Id)
	// 落库前归一：省份集合决定 dat 里的 tag，顺序不定会让同一个集合
	// 生成两个 tag。
	if got.Regions != `["江苏省","河南省"]` {
		t.Errorf("新增后 Regions = %s，期望排序去重后的结果", got.Regions)
	}

	got.Regions = `["  广东省  "]`
	if err := s.UpdateInbound(got); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}
	again := mustGet(t, in.Id)
	// UpdateInbound 是逐字段复制的，漏掉新字段会让"改了保存后没生效"静默发生。
	if again.Regions != `["广东省"]` {
		t.Errorf("修改后 Regions = %s，期望 [\"广东省\"]", again.Regions)
	}
}

func TestAddInboundRejectsCorruptRegions(t *testing.T) {
	setupGeoDB(t)
	in := &model.Inbound{
		UserId: 1, Port: 60202, Protocol: model.VLESS, Tag: "inbound-60202",
		Enable: true, Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
		Regions: `{"不是":"数组"}`,
	}
	// 落库时挡住，好过等到生成配置时才炸——那时报错指向的是配置生成，
	// 管理员很难联想到是某个入站的地区字段坏了。
	if err := (&InboundService{}).AddInbound(in); err == nil {
		t.Error("地区数据损坏时应当拒绝写入")
	}
}
