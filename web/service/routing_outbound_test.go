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
