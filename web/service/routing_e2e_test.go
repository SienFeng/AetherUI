package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// assertRealXrayAccepts 把一份配置交给真实 xray 判定，且**不做 fail open**。
// runXrayTest 的 fail open 是给生产路径用的（校验器故障不该锁住管理员），
// 测试里必须相反：拿不到判定就要让测试失败，否则这条覆盖会静默失效。
func assertRealXrayAccepts(t *testing.T, data []byte) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "a-ui-e2e-*.json")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatalf("write config: %v", err)
	}
	file.Close()

	output, runErr := exec.Command(xray.GetBinaryPath(), "run", "-test", "-c", file.Name()).CombinedOutput()
	if runErr != nil && !strings.Contains(string(output), "Configuration OK") {
		t.Fatalf("real xray rejected the generated config: %v\n%s\n--- config ---\n%s",
			runErr, output, data)
	}
}

// newE2EInbound 建一个真实 xray 会接受的入站。GenXrayInboundConfig 原样透传
// Settings/StreamSettings/Sniffing 三段 JSON，所以这里必须给出真实内容——
// VLESS 少了 decryption:"none" 会被 xray 直接拒绝。
func newE2EInbound(t *testing.T, port int, uuid string) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Enable: true,
		Tag:            "inbound-" + strconv.Itoa(port),
		Settings:       `{"clients":[{"id":"` + uuid + `"}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Sniffing:       `{"enabled":true,"destOverride":["http","tls"]}`,
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	return in
}

// newE2EGroup 建一个域名组，域名按录入顺序序列化——顺序是生成确定性的前提。
func newE2EGroup(t *testing.T, remark string, domains ...string) *model.DomainGroup {
	t.Helper()
	encoded, err := EncodeDomains(domains)
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	g := &model.DomainGroup{Remark: remark, Domains: encoded}
	if err := (&DomainGroupService{}).Add(g); err != nil {
		t.Fatalf("add group %q: %v", remark, err)
	}
	return g
}

// 这是本子系统此前唯一缺失的覆盖：没有任何测试把**完整生成配置**交给真实 xray。
// C1（保留 tag 撞名）正是因此才能通过全部审查——孤立校验发现不了组合冲突。
//
// 覆盖的是设计文档 §2「完整目标形态」那一行：模板 + 多入站 + 多出站 +
// 全局 block + 按用户 proxy，一次性交给真实 xray 判定。
func TestGeneratedConfigIsAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	// 原始需求场景：甲、乙两个用户访问同一批 ChatGPT 域名走不同落地节点，
	// 另有一批违规域名对所有人封禁。
	jia := newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	yi := newE2EInbound(t, 10002, "b831381d-6324-4d53-ad4f-8cda48b30812")

	chatgpt := newE2EGroup(t, "ChatGPT", "geosite:openai", "domain:openai.com")
	banned := newE2EGroup(t, "违规域名", "domain:example-banned.com")

	nodeService := OutboundNodeService{}
	nodeB, err := nodeService.AddFromLink("socks5://alice:secret@1.2.3.4:1080", "B 节点 香港")
	if err != nil {
		t.Fatalf("add node B: %v", err)
	}
	nodeC, err := nodeService.AddFromLink("socks5://5.6.7.8:1080", "C 节点 东京")
	if err != nil {
		t.Fatalf("add node C: %v", err)
	}

	ruleService := RoutingRuleService{}
	for _, rule := range []*model.RoutingRule{
		{Remark: "全员封禁违规域名", InboundIds: "[]", DomainGroupId: banned.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{banned.Id}),
			Action: model.ActionBlock, Priority: 0, Enable: true},
		{Remark: "甲的 ChatGPT 走 B", InboundIds: mustEncodeIds(t, []int{jia.Id}), DomainGroupId: chatgpt.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{chatgpt.Id}),
			Action: model.ActionProxy, OutboundId: nodeB.Id, Priority: 1, Enable: true},
		{Remark: "乙的 ChatGPT 走 C", InboundIds: mustEncodeIds(t, []int{yi.Id}), DomainGroupId: chatgpt.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{chatgpt.Id}),
			Action: model.ActionProxy, OutboundId: nodeC.Id, Priority: 2, Enable: true},
	} {
		if err := ruleService.Add(rule); err != nil {
			t.Fatalf("add rule %q: %v", rule.Remark, err)
		}
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)
}

// 脏数据不该让整份配置作废：任何一条残缺的规则或损坏的节点都必须被跳过，
// 剩下的配置仍然要是一份真实 xray 接受的合法配置。
func TestGeneratedConfigStaysValidWithDirtyData(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	in := newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	group := newE2EGroup(t, "ChatGPT", "geosite:openai")

	// 绕过 service 直接写库，模拟直接改库/并发删除/历史脏数据留下的残骸
	db := database.GetDB()
	for _, node := range []*model.OutboundNode{
		{Tag: model.BlockOutboundTag, Remark: "撞名保留 tag", Protocol: "socks", Enable: true,
			Config: `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}`},
		{Tag: "a-ui-null", Remark: "配置为 null", Protocol: "socks", Enable: true, Config: "null"},
		{Tag: "a-ui-broken", Remark: "配置损坏", Protocol: "socks", Enable: true, Config: "{not json"},
	} {
		if err := db.Save(node).Error; err != nil {
			t.Fatalf("save dirty node %q: %v", node.Tag, err)
		}
	}
	for _, rule := range []*model.RoutingRule{
		{Remark: "引用不存在的域名组", DomainGroupId: 999, DomainGroupIds: mustEncodeGroupIds(t, []int{999}), Action: model.ActionBlock, Enable: true},
		{Remark: "引用不存在的出站", DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: model.ActionProxy,
			OutboundId: 999, Enable: true},
		{Remark: "引用不存在的入站", DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), InboundIds: "[999]",
			Action: model.ActionBlock, Enable: true},
		{Remark: "动作未知", DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: "definitely-not-an-action", Enable: true},
		{Remark: "唯一一条好规则", DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), InboundIds: mustEncodeIds(t, []int{in.Id}),
			Action: model.ActionBlock, Enable: true},
	} {
		if err := db.Save(rule).Error; err != nil {
			t.Fatalf("save dirty rule %q: %v", rule.Remark, err)
		}
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)

	// 反向确认：那条唯一合法的规则确实还在，否则本测试可能只是因为
	// 什么都没生成而通过，从而失去区分力。
	rules := decodeRules(t, cfg)
	found := false
	for _, r := range rules {
		if r["outboundTag"] == model.BlockOutboundTag {
			found = true
		}
	}
	if !found {
		t.Error("the one well-formed rule disappeared; the test would pass vacuously")
	}
}

// 直接复刻事故路径：管理员把出站节点备注命名为「block」。
// 修复前 allocTag 会分配到 a-ui-block，与注入器始终注入的黑洞出站撞名，
// 生成的配置里出现两个 a-ui-block，xray 报 "existing tag found" 并拒绝启动，
// 全员断网，而面板首页仍显示 running、errorMsg 为空。
//
// CheckXrayRunningJob 每 30 秒重试都撞同一个冲突，不会自愈；Tag 设计上不可变，
// 唯一补救是禁用或删除该节点。所以这条路径必须在分配阶段就走不通。
func TestNodeRemarkedBlockStillProducesAValidConfig(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "block")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	if node.Tag == model.BlockOutboundTag {
		t.Fatalf("node was given the reserved tag %q", node.Tag)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)

	// 这个节点必须真的进了配置，否则测试只是因为它被跳过而通过
	if !strings.Contains(string(data), node.Tag) {
		t.Errorf("node tag %q is missing from the generated config", node.Tag)
	}
}
