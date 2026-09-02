package service

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"a-ui/database/model"
)

func TestAddFromLinkParsesSocksAndAssignsPrefixedTag(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}

	cred := base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	node, err := s.AddFromLink("socks://"+cred+"@1.2.3.4:1080#ignored", "香港 B 节点")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	if node.Protocol != "socks" {
		t.Errorf("Protocol = %q, want socks", node.Protocol)
	}
	if !strings.HasPrefix(node.Tag, model.OutboundTagPrefix+"-") {
		t.Errorf("Tag = %q, want %s- prefix", node.Tag, model.OutboundTagPrefix)
	}
	// Config 中的 tag 必须与本表 Tag 一致，否则注入后规则会引用不到。
	var cfg map[string]any
	if err := json.Unmarshal([]byte(node.Config), &cfg); err != nil {
		t.Fatalf("Config is not valid JSON: %v", err)
	}
	if cfg["tag"] != node.Tag {
		t.Errorf("Config tag = %v, want %v", cfg["tag"], node.Tag)
	}
}

func TestAddFromLinkTagsAreUniqueForSameRemark(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}

	first, err := s.AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("first AddFromLink: %v", err)
	}
	second, err := s.AddFromLink("socks5://5.6.7.8:1080", "hk")
	if err != nil {
		t.Fatalf("second AddFromLink: %v", err)
	}
	if first.Tag == second.Tag {
		t.Fatalf("tags collided: %q", first.Tag)
	}
}

func TestAddFromLinkRejectsUnsupportedScheme(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}
	if _, err := s.AddFromLink("http://example.com", "x"); err == nil {
		t.Error("expected error for unsupported scheme")
	}
}

func TestAddFromJSONRejectsInvalidJSON(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}
	if _, err := s.AddFromJSON("{not json", "x"); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestAddFromJSONRequiresProtocol(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}
	if _, err := s.AddFromJSON(`{"settings":{}}`, "x"); err == nil {
		t.Error("expected error when protocol field is missing")
	}
}

func TestGetEnabledSkipsDisabled(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}

	on, _ := s.AddFromLink("socks5://1.2.3.4:1080", "on")
	off, _ := s.AddFromLink("socks5://5.6.7.8:1080", "off")
	off.Enable = false
	if err := s.Update(off); err != nil {
		t.Fatalf("Update: %v", err)
	}

	enabled, err := s.GetEnabled()
	if err != nil {
		t.Fatalf("GetEnabled: %v", err)
	}
	if len(enabled) != 1 || enabled[0].Id != on.Id {
		t.Errorf("GetEnabled = %v, want only %d", enabled, on.Id)
	}
}

func TestUpdateKeepsTagWhenConfigEdited(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}

	node, err := s.AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	originalTag := node.Tag

	// 模拟用户在 JSON 高级模式里改了配置，并把 tag 也改成了别的值。
	// tag 必须被强制改回本表分配的值，否则引用旧 tag 的路由规则会悬空，
	// 而 xray 对悬空 outboundTag 不报错，流量会静默回落到直连。
	edited := `{"protocol":"socks","tag":"caller-supplied",` +
		`"settings":{"servers":[{"address":"9.9.9.9","port":1080}]}}`
	if err := s.Update(&model.OutboundNode{
		Id: node.Id, Remark: "hk", Enable: true, Config: edited,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reloaded, err := s.Get(node.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Tag != originalTag {
		t.Errorf("Tag = %q, want it to stay %q", reloaded.Tag, originalTag)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(reloaded.Config), &cfg); err != nil {
		t.Fatalf("Config is not valid JSON: %v", err)
	}
	if cfg["tag"] != originalTag {
		t.Errorf("Config tag = %v, want %v — a caller-supplied tag must be overwritten",
			cfg["tag"], originalTag)
	}

	// 同时确认编辑后的内容确实落库了，否则本测试可能只是因为 Update
	// 什么都没做而「保住」了 tag，从而失去区分力。
	settings, ok := cfg["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing in persisted config: %v", cfg)
	}
	servers, ok := settings["servers"].([]any)
	if !ok || len(servers) == 0 {
		t.Fatalf("servers missing in persisted config: %v", settings)
	}
	first, _ := servers[0].(map[string]any)
	if first["address"] != "9.9.9.9" {
		t.Errorf("address = %v, want 9.9.9.9 — the edited config should have been persisted",
			first["address"])
	}
}

func TestUpdateRejectsInvalidOutboundConfig(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	s := OutboundNodeService{}

	node, err := s.AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	original := node.Config

	// 模拟用户在 JSON 高级模式里把一个已存在的节点编辑成 xray 不接受的配置。
	// 必须在落库前被拒，否则整份 xray 配置会变成非法配置，xray 起不来，
	// 所有用户一起断网。
	bad := `{"protocol":"definitely-not-a-protocol","settings":{}}`
	if err := s.Update(&model.OutboundNode{
		Id: node.Id, Remark: "hk", Enable: true, Config: bad,
	}); err == nil {
		t.Fatal("Update accepted an invalid outbound config; it must be rejected before the write")
	}

	// 被拒之后，库里必须还是原来那份配置
	reloaded, err := s.Get(node.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Config != original {
		t.Errorf("stored config changed despite the rejection:\n  got  %s\n  want %s",
			reloaded.Config, original)
	}

	// 反向确认：合法的编辑仍然能通过，否则本测试可能只是因为 Update 恒失败而通过
	good := `{"protocol":"socks","settings":{"servers":[{"address":"9.9.9.9","port":1080}]}}`
	if err := s.Update(&model.OutboundNode{
		Id: node.Id, Remark: "hk", Enable: true, Config: good,
	}); err != nil {
		t.Fatalf("Update rejected a valid config: %v", err)
	}
}

// 备注写成「block」会让 SuggestTag 生成 a-ui-block —— 与注入器始终注入的
// 黑洞出站撞名。xray 报 "existing tag found" 并拒绝启动，全员断网，而面板
// 首页仍显示 running（Process.Start 把 cmd.Run 丢进 goroutine，启动失败不回传）。
// 数据库的唯一约束管不到这个冲突：注入器发出的 tag 根本不在 outbound_nodes 表里，
// 所以必须由分配端自己排除。
func TestAddFromLinkNeverMintsReservedTag(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}
	// SlugRemark 会把下面这些备注归一到同一个 slug "block"
	for _, remark := range []string{"block", "Block", "BLOCK", "block!", " block "} {
		node, err := s.AddFromLink("socks5://1.2.3.4:1080", remark)
		if err != nil {
			t.Fatalf("AddFromLink(%q): %v", remark, err)
		}
		if node.Tag == model.BlockOutboundTag {
			t.Errorf("remark %q produced the reserved tag %q", remark, node.Tag)
		}
	}
}

// json.Unmarshal([]byte("null"), &m) 不报错，但会留下一个 nil map，
// 紧接着的 m["tag"] = ... 直接 panic。走 API 是 500（gin 有 Recovery），
// 走每 10 秒的重启 cron 则会杀掉整个面板进程（cron 未配 Recover）。
func TestUpdateRejectsNullConfigWithoutPanicking(t *testing.T) {
	setupDB(t)
	s := OutboundNodeService{}
	node, err := s.AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	if err := s.Update(&model.OutboundNode{
		Id: node.Id, Remark: "hk", Enable: true, Config: "null",
	}); err == nil {
		t.Error(`expected a "null" outbound config to be rejected`)
	}
}

// tag 必须在校验之前就定下来。persist 若先校验、后分配 tag，完整配置校验
// 看到的是解析器给的旧 tag，看不见真正会写进配置的那一个，组合冲突就检不出来。
//
// 真实场景：用户在 xray 模板里手写了一个 tag 为 a-ui-hk 的出站——`a-ui-` 前缀
// 只是命名空间约定，而模板是用户可自由编辑的，拦不住这种写法。此时新建备注为
// 「hk」的节点，allocTag 查 outbound_nodes 表发现没人占用，就会分配 a-ui-hk，
// 与模板里那个撞名，xray 报 "existing tag found" 并拒绝启动，全员断网。
func TestAddRejectsTagCollidingWithATemplateOutbound(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	tmpl := `{"outbounds":[` +
		`{"protocol":"freedom","settings":{}},` +
		`{"tag":"a-ui-hk","protocol":"freedom","settings":{}}` +
		`]}`
	if err := (&SettingService{}).setString("xrayTemplateConfig", tmpl); err != nil {
		t.Fatalf("setString: %v", err)
	}
	if _, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk"); err == nil {
		t.Error("a tag colliding with an outbound already in the template must be rejected")
	}
}
