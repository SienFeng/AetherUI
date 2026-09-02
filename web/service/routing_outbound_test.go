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
