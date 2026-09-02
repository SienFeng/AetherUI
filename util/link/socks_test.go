package link

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseSocksV2rayNFormat(t *testing.T) {
	// socks://<base64(user:pass)>@host:port#remark
	cred := base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	got, err := ParseLink("socks://" + cred + "@1.2.3.4:1080#HK")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outbound["protocol"] != "socks" {
		t.Fatalf("protocol = %v, want socks", got.Outbound["protocol"])
	}
	server := firstServer(t, got.Outbound)
	if server["address"] != "1.2.3.4" {
		t.Errorf("address = %v, want 1.2.3.4", server["address"])
	}
	if server["port"] != 1080 {
		t.Errorf("port = %v, want 1080", server["port"])
	}
	users, ok := server["users"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("users = %v, want 1 entry", server["users"])
	}
	u := users[0].(map[string]any)
	if u["user"] != "alice" || u["pass"] != "secret" {
		t.Errorf("credentials = %v/%v, want alice/secret", u["user"], u["pass"])
	}
}

func TestParseSocksLegacyWholeBodyBase64(t *testing.T) {
	// socks://<base64(user:pass@host:port)>#remark
	body := base64.StdEncoding.EncodeToString([]byte("bob:pw@example.com:1081"))
	got, err := ParseLink("socks://" + body + "#tokyo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	server := firstServer(t, got.Outbound)
	if server["address"] != "example.com" || server["port"] != 1081 {
		t.Errorf("host:port = %v:%v, want example.com:1081", server["address"], server["port"])
	}
}

func TestParseSocksPlainNoAuth(t *testing.T) {
	// socks5://host:port，无凭据时不应产生 users 字段
	got, err := ParseLink("socks5://10.0.0.1:1080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	server := firstServer(t, got.Outbound)
	if server["address"] != "10.0.0.1" || server["port"] != 1080 {
		t.Errorf("host:port = %v:%v, want 10.0.0.1:1080", server["address"], server["port"])
	}
	if _, exists := server["users"]; exists {
		t.Error("users field must be absent when no credentials given")
	}
}

func TestParseSocksIPv6(t *testing.T) {
	got, err := ParseLink("socks5://[2001:db8::1]:1080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	server := firstServer(t, got.Outbound)
	if server["address"] != "2001:db8::1" {
		t.Errorf("address = %v, want 2001:db8::1", server["address"])
	}
}

func TestParseSocksDefaultPort(t *testing.T) {
	got, err := ParseLink("socks5://10.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	server := firstServer(t, got.Outbound)
	if server["port"] != 1080 {
		t.Errorf("port = %v, want default 1080", server["port"])
	}
}

func TestParseSocksRejectsEmptyHost(t *testing.T) {
	_, err := ParseLink("socks5://")
	if err == nil {
		t.Fatal("expected error for empty body")
	}
	// Assert error comes from parseSocks itself, not from ParseLink's default case
	// (which would return "unsupported link scheme") — otherwise this test would
	// pass even before socks handling was wired up.
	if !strings.Contains(err.Error(), "socks:") {
		t.Errorf("error = %q, want an error from parseSocks (prefixed \"socks:\")", err)
	}
}

func firstServer(t *testing.T, ob Outbound) map[string]any {
	t.Helper()
	settings, ok := ob["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing: %v", ob)
	}
	servers, ok := settings["servers"].([]any)
	if !ok || len(servers) == 0 {
		t.Fatalf("servers missing: %v", settings)
	}
	return servers[0].(map[string]any)
}
