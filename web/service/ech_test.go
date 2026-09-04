package service

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGetNewEchCertIsAcceptedByXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	s := ServerService{}
	keys, err := s.GetNewEchCert("")
	if err != nil {
		t.Fatalf("GetNewEchCert: %v", err)
	}

	serverKeys, _ := keys["echServerKeys"].(string)
	configList, _ := keys["echConfigList"].(string)
	if serverKeys == "" || configList == "" {
		t.Fatalf("echServerKeys/echConfigList 不能为空: %#v", keys)
	}
	// 核心用 base64.StdEncoding 解 echServerKeys（transport_security.go:392），
	// 与 REALITY 的 RawURLEncoding 不同，混用会被拒。
	if _, err := base64.StdEncoding.DecodeString(serverKeys); err != nil {
		t.Errorf("echServerKeys 不是 base64.StdEncoding: %v", err)
	}
	if _, err := base64.StdEncoding.DecodeString(configList); err != nil {
		t.Errorf("echConfigList 不是 base64.StdEncoding: %v", err)
	}

	// 简报原假设 `xray run -test` 只做配置构建校验、不读证书内容，实测证伪：
	// 会尝试 infra/conf 解析证书文件，路径不存在直接报
	// "failed to parse certificate: open ...: no such file or directory"。
	// 因此改用 t.TempDir() 里生成的自签证书。
	certFile, keyFile := writeSelfSignedCert(t)

	ib := echInbound(serverKeys, certFile, keyFile)
	if err := ValidateInboundReplacing(ib, ""); err != nil {
		t.Errorf("带 ECH 的 TLS 入站被核心拒绝: %v", err)
	}
}

// writeSelfSignedCert 生成一份自签证书写入 t.TempDir()，供 xray run -test
// 的证书解析步骤使用。
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成证书私钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.org"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("生成自签证书: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("序列化私钥: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "ech-probe.crt")
	keyFile = filepath.Join(dir, "ech-probe.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("写证书: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("写私钥: %v", err)
	}
	return certFile, keyFile
}

// TestGetNewEchCertHonoursServerName 守住 publicName 会被真正写进 ECHConfig。
// 传进去的 serverName 若被忽略,生成的 config 里仍是默认的 cloudflare-ech.com,
// 而这一层是给外部观察者看的"公开名",写错了伪装就不是用户以为的那个。
func TestGetNewEchCertHonoursServerName(t *testing.T) {
	s := ServerService{}
	a, err := s.GetNewEchCert("example.org")
	if err != nil {
		t.Fatalf("GetNewEchCert: %v", err)
	}
	b, err := s.GetNewEchCert("")
	if err != nil {
		t.Fatalf("GetNewEchCert: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(a["echConfigList"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContains(raw, []byte("example.org")) {
		t.Error("echConfigList 里没有出现传入的 serverName")
	}
	rawDefault, err := base64.StdEncoding.DecodeString(b["echConfigList"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContains(rawDefault, []byte("cloudflare-ech.com")) {
		t.Error("serverName 留空时应当回落到核心的默认值 cloudflare-ech.com")
	}
}

// TestGetNewEchCertServerKeysLayout 锁住 echServerKeys 的二进制布局
// (uint16 privLen, priv[32], uint16 configLen, config[...])。config 与
// priv 是两个对称的 uint16 长度前缀字段，顺序写反后 xray 的
// ConvertToGoECHKeys 仍能"合法"解出、run -test 也不会报错，只有握手时才
// 会失败——因此不能靠 run -test 或"能不能解出来"守住顺序，只能靠布局本身
// 的不对称性：X25519 私钥固定 32 字节，而 ECHConfig（含 KEM/HPKE 套件/
// publicName 等字段）明显更长。第一段恰好 32、第二段明显更大，就说明
// priv 在前、config 在后没有被写反。
func TestGetNewEchCertServerKeysLayout(t *testing.T) {
	s := ServerService{}
	keys, err := s.GetNewEchCert("")
	if err != nil {
		t.Fatalf("GetNewEchCert: %v", err)
	}
	// 注意：ECH 用 base64.StdEncoding，REALITY 用 RawURLEncoding，两者不能混用。
	raw, err := base64.StdEncoding.DecodeString(keys["echServerKeys"].(string))
	if err != nil {
		t.Fatalf("echServerKeys 不是合法 base64.StdEncoding: %v", err)
	}
	if len(raw) < 2 {
		t.Fatalf("echServerKeys 太短，读不出第一段长度前缀: %d bytes", len(raw))
	}
	privLen := int(raw[0])<<8 | int(raw[1])
	if privLen != 32 {
		t.Fatalf("第一段长度 = %d，期望 32（X25519 私钥固定长度）；priv/config 顺序是否被写反？", privLen)
	}
	configLenOffset := 2 + privLen
	if len(raw) < configLenOffset+2 {
		t.Fatalf("echServerKeys 太短，读不出第二段长度前缀: %d bytes", len(raw))
	}
	configLen := int(raw[configLenOffset])<<8 | int(raw[configLenOffset+1])
	if configLen <= 32 {
		t.Fatalf("第二段长度 = %d，期望明显大于 32（ECHConfig 比 X25519 私钥长）；priv/config 顺序是否被写反？", configLen)
	}
}

func bytesContains(haystack, needle []byte) bool {
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := range needle {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

func echInbound(echServerKeys, certFile, keyFile string) map[string]any {
	return map[string]any{
		"tag":      "inbound-ech-probe",
		"port":     8443,
		"protocol": "vless",
		"settings": map[string]any{
			"clients":    []any{map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "flow": ""}},
			"decryption": "none",
		},
		"streamSettings": map[string]any{
			"network":  "ws",
			"security": "tls",
			"tlsSettings": map[string]any{
				"serverName":    "example.org",
				"echServerKeys": echServerKeys,
				"certificates": []any{map[string]any{
					"certificateFile": certFile,
					"keyFile":         keyFile,
				}},
			},
			"wsSettings": map[string]any{"path": "/"},
		},
	}
}
