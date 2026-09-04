package service

import (
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"encoding/base64"
	"io"

	"golang.org/x/crypto/cryptobyte"
)

// ECH（Encrypted Client Hello）把 TLS 握手里的 SNI 也加密掉，外部观察者只能
// 看到 publicName（默认 cloudflare-ech.com），看不到真实域名。
//
// 本文件复刻 xray-core 的 main/commands/all/tls/ech.go:42-149。之所以不 exec
// `bin/xray tls ech`，理由与 x25519 相同（见 server.go 的 GetNewX25519Cert）。
//
// HPKE 套件用标准库 crypto/hpke（go.mod 锁定 go 1.27.0，该包自 Go 1.24 起随
// 标准库提供），而不是 github.com/cloudflare/circl/hpke——核心自己的
// ech.go 导入的就是 crypto/hpke，circl/hpke 是完全不同的一套类型（KDF/AEAD
// 是带显式 ID 的枚举常量，不是返回 .ID() 方法的函数），编译不过。用标准库
// 同样满足"不新增 Go 模块"的约束。
//
// 两处编码陷阱：
//   - ECH 用 base64.StdEncoding（transport_security.go:392），REALITY 用
//     RawURLEncoding，两者不能混。
//   - echConfigList 与 echServerKeys 是**两种不同的二进制结构**：前者是
//     长度前缀包着的 ECHConfig，给客户端；后者是 (privLen, priv, configLen,
//     config) 的拼接，给服务端。搞反了核心不会报错，握手时才失败。

const extensionEncryptedClientHello = 0xfe0d

type echCipher struct {
	kdfID  uint16
	aeadID uint16
}

// echSuites 与核心 ech.go:52-62 的列表逐项一致。顺序影响生成的字节，
// 但 ECH 配置不参与 Config.Equals 之外的确定性要求，此处保持与核心同序
// 只是为了便于对照排查。
func echSuites() []echCipher {
	return []echCipher{
		{hpke.HKDFSHA256().ID(), hpke.AES128GCM().ID()},
		{hpke.HKDFSHA256().ID(), hpke.AES256GCM().ID()},
		{hpke.HKDFSHA256().ID(), hpke.ChaCha20Poly1305().ID()},
		{hpke.HKDFSHA384().ID(), hpke.AES128GCM().ID()},
		{hpke.HKDFSHA384().ID(), hpke.AES256GCM().ID()},
		{hpke.HKDFSHA384().ID(), hpke.ChaCha20Poly1305().ID()},
		{hpke.HKDFSHA512().ID(), hpke.AES128GCM().ID()},
		{hpke.HKDFSHA512().ID(), hpke.AES256GCM().ID()},
		{hpke.HKDFSHA512().ID(), hpke.ChaCha20Poly1305().ID()},
	}
}

// marshalEchConfig 复刻核心 ech.go:121-146 的 marshalBinary。
func marshalEchConfig(configID uint8, kemID uint16, publicKey, publicName []byte) ([]byte, error) {
	var b cryptobyte.Builder
	b.AddUint16(extensionEncryptedClientHello)
	b.AddUint16LengthPrefixed(func(child *cryptobyte.Builder) {
		child.AddUint8(configID)
		child.AddUint16(kemID)
		child.AddUint16(uint16(len(publicKey)))
		child.AddBytes(publicKey)
		child.AddUint16LengthPrefixed(func(child *cryptobyte.Builder) {
			for _, cs := range echSuites() {
				child.AddUint16(cs.kdfID)
				child.AddUint16(cs.aeadID)
			}
		})
		child.AddUint8(0) // maxNameLength，核心固定为 0
		child.AddUint8(uint8(len(publicName)))
		child.AddBytes(publicName)
		child.AddUint16LengthPrefixed(func(child *cryptobyte.Builder) {
			// extensions：核心固定为空
		})
	})
	return b.Bytes()
}

// GetNewEchCert 生成一份 ECH 密钥。serverName 是 ECH 的 publicName——
// 它是外部观察者唯一能看到的名字，留空则用核心默认的 cloudflare-ech.com
// （对齐 ech.go:38）。
func (s *ServerService) GetNewEchCert(serverName string) (map[string]any, error) {
	if serverName == "" {
		serverName = "cloudflare-ech.com"
	}

	priv := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, priv); err != nil {
		return nil, err
	}
	privKey, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, err
	}

	kemID := hpke.DHKEM(ecdh.X25519()).ID()
	configBytes, err := marshalEchConfig(0, kemID, privKey.PublicKey().Bytes(), []byte(serverName))
	if err != nil {
		return nil, err
	}

	// 客户端要的 ECHConfigList：一个 uint16 长度前缀包住 config。
	var cl cryptobyte.Builder
	cl.AddUint16LengthPrefixed(func(child *cryptobyte.Builder) {
		child.AddBytes(configBytes)
	})
	configList, err := cl.Bytes()
	if err != nil {
		return nil, err
	}

	// 服务端要的 ECHServerKeys：(privLen, priv, configLen, config)。
	var sk cryptobyte.Builder
	sk.AddUint16(uint16(len(priv)))
	sk.AddBytes(priv)
	sk.AddUint16(uint16(len(configBytes)))
	sk.AddBytes(configBytes)
	serverKeys, err := sk.Bytes()
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"echConfigList": base64.StdEncoding.EncodeToString(configList),
		"echServerKeys": base64.StdEncoding.EncodeToString(serverKeys),
	}, nil
}
