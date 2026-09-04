package service

import (
	"encoding/base64"
	"testing"
)

// 生成的密钥必须能被真实核心接受。只断言「长度是 32 字节」不够——
// xray 的 REALITYConfig.Build（infra/conf/transport_security.go:100）会
// 解码并校验，编码方式选错（StdEncoding 而非 RawURLEncoding）时长度照样对，
// 但核心会拒绝整份配置。
func TestGetNewX25519CertIsAcceptedByXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	s := ServerService{}
	keys, err := s.GetNewX25519Cert()
	if err != nil {
		t.Fatalf("GetNewX25519Cert: %v", err)
	}

	priv, _ := keys["privateKey"].(string)
	pub, _ := keys["publicKey"].(string)
	if priv == "" || pub == "" {
		t.Fatalf("privateKey/publicKey 不能为空: %#v", keys)
	}
	if priv == pub {
		t.Fatal("privateKey 与 publicKey 相同，说明生成逻辑写错了")
	}
	for name, v := range map[string]string{"privateKey": priv, "publicKey": pub} {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			t.Errorf("%s 不是 base64.RawURLEncoding: %v", name, err)
			continue
		}
		if len(raw) != 32 {
			t.Errorf("%s 解码后 %d 字节，核心要求 32", name, len(raw))
		}
	}

	ib := realityInboundWith(priv, "0123456789abcdef")
	if err := ValidateInboundReplacing(ib, ""); err != nil {
		t.Errorf("生成的密钥组成的 REALITY 入站被核心拒绝: %v", err)
	}
}

// realityInboundWith 造一个除密钥外一切合法的 REALITY 入站。
// 端口取 443：核心的 infra/conf/xray.go:177-179 对非 443 端口会警告
// 「提高被 GFW 封锁概率」，测试没理由示范一个坏配置。
func realityInboundWith(privateKey, shortID string) map[string]any {
	return map[string]any{
		"tag":      "inbound-reality-probe",
		"port":     443,
		"protocol": "vless",
		"settings": map[string]any{
			"clients": []any{map[string]any{
				"id":   "b831381d-6324-4d53-ad4f-8cda48b30811",
				"flow": "xtls-rprx-vision",
			}},
			"decryption": "none",
		},
		"streamSettings": map[string]any{
			"network":  "tcp",
			"security": "reality",
			"realitySettings": map[string]any{
				"show":        false,
				"target":      "www.lovelive-anime.jp:443",
				"xver":        0,
				"serverNames": []any{"www.lovelive-anime.jp"},
				"privateKey":  privateKey,
				"shortIds":    []any{shortID},
			},
		},
	}
}

func TestGetNewMldsa65IsAcceptedByXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	s := ServerService{}
	x, err := s.GetNewX25519Cert()
	if err != nil {
		t.Fatalf("GetNewX25519Cert: %v", err)
	}
	keys, err := s.GetNewMldsa65()
	if err != nil {
		t.Fatalf("GetNewMldsa65: %v", err)
	}

	seed, _ := keys["seed"].(string)
	verify, _ := keys["verify"].(string)
	if seed == "" || verify == "" {
		t.Fatalf("seed/verify 不能为空: %#v", keys)
	}
	raw, err := base64.RawURLEncoding.DecodeString(seed)
	if err != nil || len(raw) != 32 {
		t.Errorf("seed 必须是 32 字节的 base64.RawURLEncoding，得到 %d 字节 err=%v", len(raw), err)
	}
	// 核心在 transport_security.go:155-157 显式拒绝 seed 与 privateKey 相同。
	if seed == x["privateKey"] {
		t.Error("seed 不得与 x25519 privateKey 相同")
	}

	ib := realityInboundWith(x["privateKey"].(string), "0123456789abcdef")
	rs := ib["streamSettings"].(map[string]any)["realitySettings"].(map[string]any)
	rs["mldsa65Seed"] = seed
	if err := ValidateInboundReplacing(ib, ""); err != nil {
		t.Errorf("带 mldsa65Seed 的 REALITY 入站被核心拒绝: %v", err)
	}
}
