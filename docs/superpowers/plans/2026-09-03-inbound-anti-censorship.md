# 入站协议抗封锁改造 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 AetherUI 的入站表单补上 REALITY、`xtls-rprx-vision`、ECH 与全套 TLS/sniffing 参数，同时清除四个会导致整机断网的死选项。

**Architecture:** 就地扩展 `web/assets/js/model/xray.js` 的现有类（不拆文件、不引入抽象层），新增一个 `reality_settings.html` 局部模板并重写 `tls_settings.html`；后端只加三个密钥生成接口，用 Go 标准库与已有的 indirect 依赖实现，不 exec `bin/xray`。Go 侧的 `Equals` 与 `hot_diff` 均无需改动（已查证）。

**Tech Stack:** Go 1.27 / Gin 1.7.1 / Vue 2 + ant-design-vue（服务端模板，无打包工具）/ xray-core 26.7.28。

**Spec:** `docs/superpowers/specs/2026-09-03-inbound-anti-censorship-design.md`

## Global Constraints

以下约束对全部任务生效，来自设计文档与 `docs/superpowers/specs/2026-09-03-modernization-roadmap.md` §4。

- **不新增任何 Go 模块。** 只把 `github.com/cloudflare/circl` 与 `golang.org/x/crypto` 从 `go.mod` 的 indirect 区提升为 direct（由 `go mod tidy` 自动完成）。
- **不改 `xray/inbound.go` 的 `Equals`**，也不改 `xray/hot_diff.go`。理由见设计文档 §2.8：`Settings`/`StreamSettings`/`Sniffing` 是 `json_util.RawMessage` 走 `bytes.Equal`，新字段天然被覆盖；`inboundUsesReality` 已在位。**出于对 CLAUDE.md 那条警告的谨慎而去改它们，是错误的。**
- **三种编码方式不能混**：x25519 与 ML-DSA-65 用 `base64.RawURLEncoding`，ECH 用 `base64.StdEncoding`。
- **验证命令**：`make verify`（= `go vet ./...` + `go test ./...` + `go build`）。单跑测试用 `go test ./web/service/ -run <名字> -v`。
- **模板改完必须跑 `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`**。`web/web.go` 的 `getHtmlTemplate` 吞掉 `ParseFS` 错误，`go build` 发现不了模板语法错误。
- **Vue 指令必须写在 `el` 指向的根元素之内**，否则是完全静默的死代码。
- **前端调试**：`XUI_DEBUG=true go run main.go`，必须在仓库根目录启动（模板与静态资源按相对路径从磁盘读）。改了 `web/assets/js` 而 `config/version` 没变时，浏览器会命中 `max-age=31536000` 强缓存——手工验证时用硬刷新（Cmd+Shift+R）。
- **没有 JS 测试基础设施**（无 `package.json`）。`xray.js` 的改动靠两条防线守：Task 3 的 Go 契约测试锁定生成的 JSON 形状，加上每个前端任务末尾的手工验证步骤。**不得为此引入 npm 工具链。**
- **提交格式**：Conventional Commits，中文正文说明「为什么」。每条提交消息结尾加：
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
  ```
- **临时验证产物**一律放 `/private/tmp/claude-501/-Users-caryallen-Desktop-AetherUI-AetherUI-main/f69813d8-e9e6-460b-bffa-cdb27820aef4/scratchpad`，不进仓库。每个任务提交前跑 `git status` 核对。
- **一个测试必须在没有其修复时失败。** 每个任务的 Step 2 都要求先看到红，再实现。两边都过的测试比没有测试更糟。

---

### Task 1: REALITY 密钥生成后端（x25519 + ML-DSA-65）

**Files:**
- Modify: `web/service/server.go`（当前 301 行，在文件末尾追加）
- Modify: `web/controller/server.go:32-38`（`initRouter`）
- Test: `web/service/keygen_test.go`（新建）

**Interfaces:**
- Produces: `func (s *ServerService) GetNewX25519Cert() (map[string]any, error)`，返回 `{"privateKey": string, "publicKey": string}`
- Produces: `func (s *ServerService) GetNewMldsa65() (map[string]any, error)`，返回 `{"seed": string, "verify": string}`
- Produces: HTTP `POST {basePath}server/getNewX25519Cert` 与 `POST {basePath}server/getNewMldsa65`，响应体为 `entity.Msg`（`{success, msg, obj}`）
- Consumes: `web/service/routing_validate.go:277` 的 `ValidateInboundReplacing(ib map[string]any, replacedTag string) error`

- [ ] **Step 1: 写会失败的测试**

创建 `web/service/keygen_test.go`：

```go
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
```

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./web/service/ -run 'TestGetNewX25519CertIsAcceptedByXray|TestGetNewMldsa65IsAcceptedByXray' -v
```

期望：编译失败，`s.GetNewX25519Cert undefined` / `s.GetNewMldsa65 undefined`。

- [ ] **Step 3: 实现**

在 `web/service/server.go` 末尾追加：

```go
// GetNewX25519Cert 生成一对 REALITY 用的 X25519 密钥。
//
// 复刻 xray-core 的 main/commands/all/curve25519.go:38-58。两个易错点：
//   1. 私钥必须按 https://cr.yp.to/ecdh.html 做 clamping，漏掉会生成出
//      核心不接受的私钥；
//   2. 编码必须是 base64.RawURLEncoding，与核心 REALITYConfig.Build
//      （infra/conf/transport_security.go:100）的解码方式一致。用 StdEncoding
//      长度照样是 32 字节，但核心会拒绝整份配置。
//
// 不 exec `bin/xray x25519`：bin/xray-darwin-arm64 在 .gitignore 中，本地开发
// 环境没有该文件。密钥生成与配置校验不同，不能 fail open——生成不出来就是
// 生成不出来，不能放行一个空密钥。
func (s *ServerService) GetNewX25519Cert() (map[string]any, error) {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return nil, err
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"privateKey": base64.RawURLEncoding.EncodeToString(priv),
		"publicKey":  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
	}, nil
}

// GetNewMldsa65 生成 REALITY 的 ML-DSA-65 后量子签名密钥对。
// 复刻 main/commands/all/mldsa65.go:30-46。seed 落进入站的 realitySettings.mldsa65Seed，
// verify 落进分享链接的 pqv 参数。
func (s *ServerService) GetNewMldsa65() (map[string]any, error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	pub, _ := mldsa65.NewKeyFromSeed(&seed)
	return map[string]any{
		"seed":   base64.RawURLEncoding.EncodeToString(seed[:]),
		"verify": base64.RawURLEncoding.EncodeToString(pub.Bytes()),
	}, nil
}
```

在 `web/service/server.go` 的 import 块中加入（注意该文件已把 `github.com/shirou/gopsutil/net` 导入为 `net`，新增的这几个都不冲突）：

```go
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go mod tidy
go test ./web/service/ -run 'TestGetNewX25519CertIsAcceptedByXray|TestGetNewMldsa65IsAcceptedByXray' -v
```

期望：两个测试 PASS。若显示 `SKIP`，说明 `bin/xray-<GOOS>-<GOARCH>` 不在位——本机应当在位，缺失时先补齐再继续，不要把 SKIP 当通过。

确认 `go mod tidy` 只把 `cloudflare/circl` 移出 indirect 区，没有新增模块：

```bash
git diff go.mod
```

- [ ] **Step 5: 接上 HTTP 路由**

在 `web/controller/server.go` 的 `initRouter` 中，`g.POST("/installXray/:version", a.installXray)` 之后追加：

```go
	g.POST("/getNewX25519Cert", a.getNewX25519Cert)
	g.POST("/getNewMldsa65", a.getNewMldsa65)
```

在该文件末尾追加：

```go
func (a *ServerController) getNewX25519Cert(c *gin.Context) {
	cert, err := a.serverService.GetNewX25519Cert()
	if err != nil {
		jsonMsg(c, "生成 REALITY 密钥", err)
		return
	}
	jsonObj(c, cert, nil)
}

func (a *ServerController) getNewMldsa65(c *gin.Context) {
	keys, err := a.serverService.GetNewMldsa65()
	if err != nil {
		jsonMsg(c, "生成 ML-DSA-65 密钥", err)
		return
	}
	jsonObj(c, keys, nil)
}
```

这两个路由挂在 `g.Use(a.checkLogin)` 之后，因此天然需要登录。

- [ ] **Step 6: 全量验证**

```bash
make verify
```

期望：vet、test、build 全过。

- [ ] **Step 7: 提交**

```bash
git add web/service/server.go web/service/keygen_test.go web/controller/server.go go.mod go.sum
git commit -m "$(cat <<'MSG'
feat(server): 新增 REALITY 的 x25519 与 ML-DSA-65 密钥生成接口

面板此前没有任何密钥生成能力，Reality 因此无法配置。用 Go 标准库的
crypto/ecdh 复刻核心的 curve25519.go，而不是 exec bin/xray x25519——
bin/xray-darwin-arm64 在 .gitignore 中，本地开发环境没有该文件，而密钥
生成不同于配置校验，不能 fail open。

编码固定 base64.RawURLEncoding：用 StdEncoding 长度同样是 32 字节，
核心却会拒绝整份配置，是个不看源码就发现不了的坑。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 2: ECH 密钥生成后端

**Files:**
- Create: `web/service/ech.go`
- Modify: `web/controller/server.go`（`initRouter` 与文件末尾）
- Test: `web/service/ech_test.go`

**Interfaces:**
- Produces: `func (s *ServerService) GetNewEchCert(serverName string) (map[string]any, error)`，返回 `{"echServerKeys": string, "echConfigList": string}`（均为 `base64.StdEncoding`）
- Produces: HTTP `POST {basePath}server/getNewEchCert`，接受表单字段 `serverName`（可空）
- Consumes: Task 1 的 `ValidateInboundReplacing` 用法

ECH 单独成文件而不是塞进 `server.go`：它需要 `cryptobyte` 的二进制序列化与 HPKE 套件表，约 90 行且与系统状态采集毫无关系，混在 `server.go` 里会让那个文件失去单一职责。

- [ ] **Step 1: 写会失败的测试**

创建 `web/service/ech_test.go`：

```go
package service

import (
	"encoding/base64"
	"testing"
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

	ib := echInbound(serverKeys)
	if err := ValidateInboundReplacing(ib, ""); err != nil {
		t.Errorf("带 ECH 的 TLS 入站被核心拒绝: %v", err)
	}
}

// TestGetNewEchCertHonoursServerName 守住 publicName 会被真正写进 ECHConfig。
// 传进去的 serverName 若被忽略，生成的 config 里仍是默认的 cloudflare-ech.com，
// 而这一层是给外部观察者看的"公开名"，写错了伪装就不是用户以为的那个。
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

func echInbound(echServerKeys string) map[string]any {
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
					"certificateFile": "testdata-not-read-by-config-test.crt",
					"keyFile":         "testdata-not-read-by-config-test.key",
				}},
			},
			"wsSettings": map[string]any{"path": "/"},
		},
	}
}
```

注意 `echInbound` 里的证书路径是不存在的文件：`xray run -test` 只做**配置构建**校验，不读取证书内容。若这一点在执行时被证伪（测试报证书读取失败），改用 `t.TempDir()` 里生成的自签证书，并在测试里说明原因。

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./web/service/ -run 'TestGetNewEchCert' -v
```

期望：编译失败，`s.GetNewEchCert undefined`。

- [ ] **Step 3: 实现**

创建 `web/service/ech.go`：

```go
package service

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"

	"crypto/hpke"

	"golang.org/x/crypto/cryptobyte"
)

// ECH（Encrypted Client Hello）把 TLS 握手里的 SNI 也加密掉，外部观察者只能
// 看到 publicName（默认 cloudflare-ech.com），看不到真实域名。
//
// 本文件复刻 xray-core 的 main/commands/all/tls/ech.go:42-149。之所以不 exec
// `bin/xray tls ech`，理由与 x25519 相同（见 server.go 的 GetNewX25519Cert）。
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
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go mod tidy
go test ./web/service/ -run 'TestGetNewEchCert' -v
```

期望：两个测试 PASS。

**交叉验证（必做）**：生成的 `echServerKeys` 必须能被核心自己的 `tls ech -i` 还原。这是唯一能证明二进制布局没写反的检查——布局写反时 `run -test` 照样 `Configuration OK`，要到真正握手时才失败。

在 `web/service/ech_test.go` 里加一个把值打出来的临时用例：

```go
func TestPrintEchForManualCrossCheck(t *testing.T) {
	keys, err := (&ServerService{}).GetNewEchCert("")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("echServerKeys=%s", keys["echServerKeys"])
	t.Logf("echConfigList=%s", keys["echConfigList"])
}
```

```bash
go test ./web/service/ -run TestPrintEchForManualCrossCheck -v
./bin/xray-darwin-arm64 tls ech -i "<上一步日志里的 echServerKeys>"
```

期望：核心输出 `ECH config list:` 与 `ECH server keys:` 两行，且它打印的 config list 与我们生成的 `echConfigList` **逐字符相同**。若报 `Failed to decode ECHServerKeys`，说明 `sk` 的字节布局写反了，回到 Step 3 对照核心的 `ech.go:63-67`。

交叉验证通过后**删掉 `TestPrintEchForManualCrossCheck`**——它是一次性排查工具，不是回归测试，留在仓库里只会在每次跑测试时刷无用日志。

- [ ] **Step 5: 接上 HTTP 路由**

`web/controller/server.go` 的 `initRouter` 追加：

```go
	g.POST("/getNewEchCert", a.getNewEchCert)
```

文件末尾追加：

```go
func (a *ServerController) getNewEchCert(c *gin.Context) {
	keys, err := a.serverService.GetNewEchCert(c.PostForm("serverName"))
	if err != nil {
		jsonMsg(c, "生成 ECH 密钥", err)
		return
	}
	jsonObj(c, keys, nil)
}
```

- [ ] **Step 6: 全量验证并提交**

```bash
make verify
git status   # 确认没有临时产物混进来
git add web/service/ech.go web/service/ech_test.go web/controller/server.go go.mod go.sum
git commit -m "$(cat <<'MSG'
feat(server): 新增 ECH 密钥生成接口

ECH 把 TLS 握手里的 SNI 也加密，外部只看得到 publicName。核心早已支持
echServerKeys / echConfigList，面板此前无法生成这份密钥。

实现里两处容易写错、且写错了核心也不报错的地方都加了注释：ECH 用
StdEncoding 而 REALITY 用 RawURLEncoding；configList 与 serverKeys 是两种
不同的二进制布局，搞反了要到握手时才失败。测试用核心自己的
`xray tls ech -i` 反向还原做交叉验证。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 3: 契约测试——锁定前端将要生成的 JSON

**Files:**
- Test: `web/service/inbound_stream_contract_test.go`（新建）

**Interfaces:**
- Consumes: Task 1 的 `ServerService.GetNewX25519Cert`、Task 1 测试里的 `realityInboundWith`、`web/service/inbound_validate_test.go:25` 的 `newInboundFor(port int, stream string, enable bool) *model.Inbound`
- Produces: 无生产代码。这份测试是 Task 4–7 改 `xray.js` 时唯一的自动化防线——没有 JS 测试运行器，前端生成的 JSON 形状只能在 Go 侧锁住。

这个任务**不写任何生产代码**，只写测试。它有两个作用：一是证明四个死选项确实会被核心拒绝（把设计文档 §1.1 的论断变成可执行的证据），二是把 Task 4–7 要生成的目标 JSON 提前固定下来。

- [ ] **Step 1: 写测试**

创建 `web/service/inbound_stream_contract_test.go`：

```go
package service

import (
	"strings"
	"testing"
)

// 这四个选项今天仍然摆在面板的下拉里（web/html/xui/form/stream/stream_settings.html:9-10
// 与 web/assets/js/model/xray.js:43-46），但当前核心在配置构建阶段就会拒绝。
// xray 加载配置是全有或全无：任何一个入站用了它们，整机所有用户一起断网。
//
// 这份表驱动测试是「必须把它们从界面上删掉」的可执行证据。Task 4 与 Task 8
// 删掉它们之后，这个测试仍然应当通过——它锁的是核心的行为，不是面板的行为。
func TestRemovedTransportsAreRejectedByCore(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	cases := []struct {
		name   string
		stream map[string]any
		hint   string
	}{
		{
			name:   "network=http",
			hint:   "HTTP transport",
			stream: map[string]any{"network": "http", "security": "none", "httpSettings": map[string]any{"path": "/"}},
		},
		{
			name:   "network=quic",
			hint:   "QUIC transport",
			stream: map[string]any{"network": "quic", "security": "none", "quicSettings": map[string]any{"security": "none", "key": ""}},
		},
		{
			name: "security=xtls",
			hint: "XTLS",
			stream: map[string]any{"network": "tcp", "security": "xtls",
				"xtlsSettings": map[string]any{"serverName": "example.org"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ib := map[string]any{
				"tag": "inbound-dead-" + tc.name, "port": 44401, "protocol": "vless",
				"settings": map[string]any{
					"clients":    []any{map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "flow": ""}},
					"decryption": "none",
				},
				"streamSettings": tc.stream,
			}
			err := ValidateInboundReplacing(ib, "")
			if err == nil {
				t.Fatalf("%s 应当被核心拒绝——它还留在面板下拉里，用户选中就会导致全机断网", tc.name)
			}
			t.Logf("核心的拒绝理由: %v", err)
		})
	}
}

// 旧的 flow 值走的是另一条代码路径（infra/conf/vless.go:51 的白名单），
// 单独一个用例。
func TestRemovedFlowValuesAreRejectedByCore(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	for _, flow := range []string{"xtls-rprx-origin", "xtls-rprx-direct"} {
		t.Run(flow, func(t *testing.T) {
			ib := map[string]any{
				"tag": "inbound-flow-probe", "port": 44402, "protocol": "vless",
				"settings": map[string]any{
					"clients":    []any{map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "flow": flow}},
					"decryption": "none",
				},
				"streamSettings": map[string]any{"network": "tcp", "security": "none"},
			}
			if err := ValidateInboundReplacing(ib, ""); err == nil {
				t.Fatalf("flow=%s 应当被核心拒绝", flow)
			}
		})
	}
}

// 以下三个是 Task 4-7 改完 xray.js 之后，前端必须能生成出来的形状。
// 任何一个在这里过不了，说明目标形状本身就是错的，不必等到手工验证。

func TestRealityVisionContractIsAccepted(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	keys, err := (&ServerService{}).GetNewX25519Cert()
	if err != nil {
		t.Fatal(err)
	}
	ib := realityInboundWith(keys["privateKey"].(string), "0123456789abcdef")
	// 前端会把客户端半边参数放在 realitySettings.settings 里（3x-ui 的约定）。
	// 核心的 REALITYConfig 没有这个字段，实测确认它被忽略而不是被拒绝。
	rs := ib["streamSettings"].(map[string]any)["realitySettings"].(map[string]any)
	rs["settings"] = map[string]any{
		"publicKey": keys["publicKey"], "fingerprint": "chrome",
		"serverName": "", "spiderX": "/",
	}
	if err := ValidateInboundReplacing(ib, ""); err != nil {
		t.Fatalf("REALITY+Vision 的目标形状被拒绝: %v", err)
	}
}

// Vision 配普通 TLS 时，minVersion 必须是 1.3：核心在运行期才检查
// （proxy/vless/inbound/inbound.go:573），run -test 查不出来。所以这个测试
// 只能守住「配置合法」，TLS 1.3 的强制要靠表单（Task 11）。
// 这条限制写在这里，是为了让改表单的人知道为什么不能只依赖后端校验。
func TestTlsVisionContractIsAccepted(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	ib := map[string]any{
		"tag": "inbound-tls-vision", "port": 44403, "protocol": "vless",
		"settings": map[string]any{
			"clients":    []any{map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "flow": "xtls-rprx-vision"}},
			"decryption": "none",
		},
		"streamSettings": map[string]any{
			"network": "tcp", "security": "tls",
			"tlsSettings": map[string]any{
				"serverName": "example.org",
				"minVersion": "1.3", "maxVersion": "1.3",
				"alpn":             []any{"h2", "http/1.1"},
				"rejectUnknownSni": false,
				"certificates": []any{map[string]any{
					"certificateFile": "cert.crt", "keyFile": "private.key",
					"ocspStapling": 3600,
				}},
				"settings": map[string]any{"fingerprint": "chrome", "allowInsecure": false},
			},
			"tcpSettings": map[string]any{"header": map[string]any{"type": "none"}},
		},
	}
	if err := ValidateInboundReplacing(ib, ""); err != nil {
		t.Fatalf("TLS+Vision 的目标形状被拒绝: %v", err)
	}
}

// tlsSettings.settings 是面板自己加的客户端半边（fingerprint / allowInsecure /
// echConfigList），核心的 TLSConfig 里没有这个键。和 realitySettings.settings
// 一样，必须确认它被忽略而不是被拒绝，否则整个模型设计要改。
func TestPanelOnlySettingsKeyIsIgnoredByCore(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	stream := `{"network":"ws","security":"tls",` +
		`"tlsSettings":{"serverName":"example.org","alpn":["h2","http/1.1"],` +
		`"minVersion":"1.2","maxVersion":"1.3",` +
		`"certificates":[{"certificateFile":"cert.crt","keyFile":"private.key","ocspStapling":3600}],` +
		`"settings":{"fingerprint":"chrome","allowInsecure":false,"echConfigList":""}},` +
		`"wsSettings":{"path":"/","headers":{}}}`

	in := newInboundFor(44404, stream, true)
	if err := (&InboundService{}).AddInbound(in); err != nil {
		if strings.Contains(err.Error(), "xray") {
			t.Fatalf("面板自用的 settings 键被核心拒绝了，模型设计需要改: %v", err)
		}
		t.Fatalf("AddInbound: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试**

```bash
go test ./web/service/ -run 'TestRemovedTransports|TestRemovedFlowValues|TestRealityVisionContract|TestTlsVisionContract|TestPanelOnlySettingsKey' -v
```

期望：**全部 PASS**。

这个任务与其他任务不同，测试一开始就应当是绿的——它锁的是核心的既有行为，不是我们要新增的行为。如果 `TestRemovedTransports` 里有任何一个 case **没有**被拒绝，说明设计文档 §1.1 的判断错了，**立刻停下来**，把实际结果记录下来并回到设计评审，不要继续往下做。

如果 `TestPanelOnlySettingsKey` 失败，说明 `tlsSettings.settings` 这个面板私有键会被核心拒绝，Task 5 的模型设计必须改（改成把客户端参数存到数据库的独立列，而不是塞进 streamSettings）。同样立刻停下来。

- [ ] **Step 3: 提交**

```bash
git add web/service/inbound_stream_contract_test.go
git commit -m "$(cat <<'MSG'
test(service): 锁定入站 streamSettings 的契约

两个作用：把「面板下拉里那四个选项会被核心拒绝」从论断变成可执行的证据；
以及把改 xray.js 时要生成的目标 JSON 形状提前固定下来。

项目没有 JS 测试运行器，xray.js 的改动无法直接单测，这份 Go 测试是唯一
的自动化防线。其中两个用例专门确认 realitySettings.settings 与
tlsSettings.settings 这两个面板私有键被核心忽略而非拒绝——整个前端模型
设计建立在这个前提上。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 4: 前端模型——常量表与死选项清除

**Files:**
- Modify: `web/assets/js/model/xray.js:43-46`（`FLOW_CONTROL`）、`:50-53`（`Object.freeze` 区）、`:334-393`（`HttpStreamSettings` 与 `QuicStreamSettings`）、`:488-570`（`StreamSettings`）、`:624-655`（`Inbound` 的 `tls`/`xtls` 访问器）、`:811-843`（`canEnableTls` / `canEnableXTls`）
- Modify: `web/html/xui/inbound_modal.html:69-80`（Vue 实例的 `data`，见 Step 6）

**Interfaces:**
- Produces: 常量 `FLOW_CONTROL`（只含 `VISION`）、`TLS_VERSION_OPTION`、`TLS_CIPHER_OPTION`、`UTLS_FINGERPRINT`、`ALPN_OPTION`、`SNIFFING_OPTION`、`REALITY_TARGET_PRESETS`
- Produces: `Inbound.prototype.canEnableReality()`、`canEnableVision()`，供 Task 6 与 Task 10/11 使用
- 删除：`StreamSettings.isXTls`、`Inbound.xtls`、`Inbound.canEnableXTls`、`HttpStreamSettings`、`QuicStreamSettings`、`Inbound.isQuic`、`Inbound.quicSecurity`/`quicKey`/`quicType`

本任务只删与加常量，**不新增 REALITY 类**（那是 Task 6）。拆开是为了让删除动作能单独被审阅——删错一个 getter 会让某个协议表单静默变成死代码。

- [ ] **Step 1: 复核伪装目标预置列表（先做，因为常量表要用）**

设计文档 §4.4 规定候选值必须实测，不得凭记忆写死。下面五个域名已于 2026-09-03 实测通过，但域名的 TLS 配置会变，动手前**重跑一遍确认**：

```bash
for d in www.lovelive-anime.jp www.amazon.co.jp www.tesla.com www.cloudflare.com www.nicovideo.jp; do
  echo "=== $d ==="
  echo | timeout 12 openssl s_client -connect "$d:443" -servername "$d" -alpn h2 -tls1_3 2>&1 \
    | grep -iE '^\s*Protocol\s*:|ALPN protocol|Verify return code|Negotiated TLS1.3 group|Server Temp Key' | sed 's/^ *//'
done
```

判据（来自 3x-ui `internal/web/service/reality_scan.go:298` 的 `Feasible`）四项全过才留下：

| 判据 | 期望输出 |
|---|---|
| TLS 1.3 | `Protocol: TLSv1.3` |
| HTTP/2 | `ALPN protocol: h2` |
| X25519 系密钥交换 | `Negotiated TLS1.3 group: X25519` 或 `X25519MLKEM768` |
| 证书有效 | `Verify return code: 0 (ok)` |

2026-09-03 的实测结果：五个域名全部满足，密钥交换均为 `X25519MLKEM768`。`player.twitch.tv` 因协商不出 ALPN h2 被淘汰。

同时逐个核对不命中核心的高风险判定（`infra/conf/transport_security.go:164-170`）：非 `.ru`/`.ir`/`.cn` 后缀，不含 `apple`/`icloud`/`microsoft`。五个域名均通过。

**把本次复核的输出粘进提交消息。** 有域名不再满足判据就从列表里删掉，不要硬留。

- [ ] **Step 2: 改常量区**

把 `web/assets/js/model/xray.js:43-46` 的 `FLOW_CONTROL` 整体替换为：

```js
// 当前 Xray 核心（infra/conf/vless.go:51）只接受 "" 与 xtls-rprx-vision。
// 旧的 xtls-rprx-origin / xtls-rprx-direct 已被移除，填了会让整份配置加载失败。
const FLOW_CONTROL = {
    VISION: "xtls-rprx-vision",
};

const TLS_VERSION_OPTION = ["1.0", "1.1", "1.2", "1.3"];

// 只列真正生效的 TLS 1.2 套件。核心把这个串按 ":" 切开逐个查表，
// 查不到的**静默丢弃**（transport/internet/tls/config.go:459-463，没有
// else 分支），所以界面必须是下拉多选而不是自由文本框。
// 另外 Go 的 crypto/tls 不接受 TLS 1.3 的套件配置——Vision 与 REALITY 都走
// 1.3，这一项对它们完全无效。
const TLS_CIPHER_OPTION = [
    "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
    "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
    "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
    "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
    "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
    "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
];

// 不含 unsafe / hellogolang：核心在 transport_security.go:181 拒绝这两个值。
// 注意那段校验只在 REALITY 作为**出站**时生效，入站侧核心根本不读这个字段，
// 所以 xray run -test 验不出来，只能靠这个列表挡住。
const UTLS_FINGERPRINT = [
    "chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq",
    "random", "randomized",
];

const ALPN_OPTION = ["h3", "h2", "http/1.1"];

const SNIFFING_OPTION = ["http", "tls", "quic", "fakedns"];

// 伪装目标候选。四项判据（TLS1.3 / ALPN h2 / X25519 系密钥交换 / 证书有效）
// 已于 2026-09-03 逐个实测确认，且均不命中核心的高风险判定
// （transport_security.go:164-170：.ru/.ir/.cn 后缀，含 apple/icloud/microsoft）。
// 五个域名当时都协商出 X25519MLKEM768，满足 3x-ui reality_scan.go:298 的判据。
// 复核方式见本计划 Task 4 Step 1；域名的 TLS 配置会变，隔一段时间要重测。
//
// player.twitch.tv 曾是候选，因协商不出 ALPN h2 被淘汰——不要再加回来。
const REALITY_TARGET_PRESETS = [
    "www.lovelive-anime.jp",
    "www.amazon.co.jp",
    "www.tesla.com",
    "www.cloudflare.com",
    "www.nicovideo.jp",
];
```

`Object.freeze` 区（原 `:50-53`）补上新常量：

```js
Object.freeze(Protocols);
Object.freeze(VmessMethods);
Object.freeze(SSMethods);
Object.freeze(RULE_IP);
Object.freeze(RULE_DOMAIN);
Object.freeze(FLOW_CONTROL);
Object.freeze(TLS_VERSION_OPTION);
Object.freeze(TLS_CIPHER_OPTION);
Object.freeze(UTLS_FINGERPRINT);
Object.freeze(ALPN_OPTION);
Object.freeze(SNIFFING_OPTION);
Object.freeze(REALITY_TARGET_PRESETS);
```

- [ ] **Step 3: 删掉 http / quic 两个传输类**

删除 `class HttpStreamSettings`（`:334-365`）与 `class QuicStreamSettings`（`:367-393`）两个类的完整定义。

在 `StreamSettings` 中：构造函数删掉 `httpSettings` 与 `quicSettings` 两个形参及对应的 `this.http` / `this.quic` 赋值；`fromJson` 删掉对应两行；`toJson` 删掉 `httpSettings` 与 `quicSettings` 两行。

在 `Inbound` 中删除 `get isQuic()`、`get quicSecurity()`、`get quicKey()`、`get quicType()`，并把 `get host()` 与 `get path()` 里 `this.isH2` 的分支删掉（`isH2` 依赖 `this.stream.http`，已不存在）。

- [ ] **Step 4: 删掉 xtls 相关**

`StreamSettings` 删除 `get isXTls()` / `set isXTls()`（`:523-533`）与 `toJson()` 里的 `xtlsSettings` 一行；`fromJson()` 里 `json.security === "xtls"` 的分支改为统一读 `json.tlsSettings`。

`Inbound` 删除 `get xtls()` / `set xtls()`（`:640-655`）与 `canEnableXTls()`（`:838-843`）。

`Inbound` 的 `set tls()` 原本在关闭 TLS 时对 TROJAN 回落到 `this.xtls = true`，这条要改掉——`xtls` 已不存在：

```js
    set tls(isTls) {
        this.stream.security = isTls ? 'tls' : 'none';
    }
```

`get serverName()`（`:759`）把 `|| this.stream.isXTls` 去掉。

- [ ] **Step 5: 收紧 canEnableTls，新增 REALITY 与 Vision 的判定**

把 `canEnableTls()`（`:811-836`）里的 network 分支中的 `"http"` 与 `"quic"` 两行删掉，并在 `canSetTls()` 之后追加：

```js
    // REALITY 只支持 RAW(tcp) / XHTTP / gRPC（infra/conf/transport_internet.go:100）。
    // 本项目不做 XHTTP，因此只剩 tcp 与 grpc。
    canEnableReality() {
        switch (this.protocol) {
            case Protocols.VLESS:
            case Protocols.TROJAN:
                break;
            default:
                return false;
        }
        return this.network === "tcp" || this.network === "grpc";
    }

    // Vision 只对 VLESS 有效，且外层必须是 TLS 1.3 或 REALITY
    // （proxy/vless/inbound/inbound.go:573 在运行期检查，run -test 查不出来）。
    canEnableVision() {
        if (this.protocol !== Protocols.VLESS) {
            return false;
        }
        return this.stream.security === 'tls' || this.stream.security === 'reality';
    }
```

- [ ] **Step 6: 把新常量挂进入站弹窗的 Vue 实例（漏了会完全静默失效）**

`xray.js` 里的常量是全局的，但 **Vue 模板只能访问 `data` / `computed` / `methods` 里的成员**。`v-for="f in UTLS_FINGERPRINT"` 若找不到该成员，Vue 2 渲染出一个**空下拉**，控制台不报任何错——和「选项列表就是空的」无法区分。

现有代码已经在这么做了：`web/html/xui/inbound_modal.html:72-74` 把 `Protocols` 与 `SSMethods` 挂进了 `data`。照它加：

```js
    new Vue({
        delimiters: ['[[', ']]'],
        el: '#inbound-modal',
        data: {
            inModal: inModal,
            Protocols: protocols,
            SSMethods: SSMethods,
            FLOW_CONTROL: FLOW_CONTROL,
            TLS_VERSION_OPTION: TLS_VERSION_OPTION,
            TLS_CIPHER_OPTION: TLS_CIPHER_OPTION,
            UTLS_FINGERPRINT: UTLS_FINGERPRINT,
            ALPN_OPTION: ALPN_OPTION,
            SNIFFING_OPTION: SNIFFING_OPTION,
            REALITY_TARGET_PRESETS: REALITY_TARGET_PRESETS,
            echLoading: false,
            realityLoading: false,
            mldsaLoading: false,
            get inbound() {
                return inModal.inbound;
            },
            // ……以下保持原样
```

`echLoading` / `realityLoading` / `mldsaLoading` 是 Task 9、Task 10 的按钮要用的加载态，一并在这里加好，后面两个任务就不必再动这个 `data` 块。

注意这个 `data` 是一个**普通对象字面量**（不是函数），且里面混用了 getter——加普通字段没问题，但不要把新字段写成 getter，否则 Vue 无法对它做响应式赋值。

- [ ] **Step 7: 语法检查**

没有 JS 测试运行器，但至少要确认文件能被解析：

```bash
node --check web/assets/js/model/xray.js && echo "语法 OK"
```

若本机没有 node，改用浏览器控制台：`XUI_DEBUG=true go run main.go` 启动后打开入站页，控制台不得有 `SyntaxError`。

- [ ] **Step 8: 手工验证**

```bash
XUI_DEBUG=true go run main.go
```

浏览器打开面板 → 入站 → 添加入站（硬刷新 Cmd+Shift+R 绕开强缓存）。确认：

1. 传输下拉里**仍然**有 http/quic（模板还没改，那是 Task 8）——但选中它们时页面不再崩溃（对应的类已删，模板的 `v-if` 分支不会渲染）。若控制台报 `Cannot read property of undefined`，说明 Step 3 有遗漏。
2. 协议选 vless 时不再出现 xtls 开关。
3. 控制台无报错。

- [ ] **Step 9: 回归与提交**

```bash
go test ./web/service/ -run 'TestRemovedTransports|TestRemovedFlowValues' -v
make verify
git add web/assets/js/model/xray.js web/html/xui/inbound_modal.html
git commit -m "$(cat <<'MSG'
refactor(xray.js): 删除核心已移除的 http/quic 传输与 xtls 安全层，补齐常量表

这四个选项（network=http、network=quic、security=xtls、旧的两个 flow 值）
在 Xray 26.7.28 上会让整份配置加载失败——xray 加载配置是全有或全无，
一个入站用了它们，机器上所有用户一起断网。web/service/inbound_stream_contract_test.go
是这一点的可执行证据。

新增的常量表里有两处反直觉的地方，注释里写明了：cipherSuites 的无效名字
会被核心静默丢弃且对 TLS 1.3 完全无效；uTLS 指纹的合法性核心在入站侧
根本不校验，只能靠前端列表挡住。

伪装目标预置列表的四项判据实测结果见下：
<粘贴 Step 1 的实测输出>

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 5: 前端模型——TlsStreamSettings 扩展

**Files:**
- Modify: `web/assets/js/model/xray.js:412-486`（`TlsStreamSettings` 与 `TlsStreamSettings.Cert`）

**Interfaces:**
- Consumes: Task 4 的 `TLS_VERSION_OPTION`、`ALPN_OPTION`、`UTLS_FINGERPRINT`
- Produces: `TlsStreamSettings` 新字段 `alpn`（数组）、`minVersion`、`maxVersion`、`cipherSuites`、`rejectUnknownSni`、`echServerKeys`，以及 `settings` 子对象（`TlsStreamSettings.Settings`，含 `fingerprint`、`allowInsecure`、`echConfigList`）
- Produces: `TlsStreamSettings.Cert` 新字段 `ocspStapling`
- 供 Task 9（模板）与 Task 12（分享链接）使用

- [ ] **Step 1: 替换 TlsStreamSettings**

把 `web/assets/js/model/xray.js:412-450` 的 `class TlsStreamSettings` 整体替换为：

```js
class TlsStreamSettings extends XrayCommonClass {
    constructor(serverName='',
                minVersion='1.2',
                maxVersion='1.3',
                cipherSuites='',
                rejectUnknownSni=false,
                alpn=['h2', 'http/1.1'],
                echServerKeys='',
                certificates=[new TlsStreamSettings.Cert()],
                settings=new TlsStreamSettings.Settings()) {
        super();
        this.server = serverName;
        this.minVersion = minVersion;
        this.maxVersion = maxVersion;
        this.cipherSuites = cipherSuites;
        this.rejectUnknownSni = rejectUnknownSni;
        this.alpn = alpn;
        this.echServerKeys = echServerKeys;
        this.certs = certificates;
        this.settings = settings;
    }

    addCert(cert) {
        this.certs.push(cert);
    }

    removeCert(index) {
        this.certs.splice(index, 1);
    }

    static fromJson(json={}) {
        let certs;
        if (!ObjectUtil.isEmpty(json.certificates)) {
            certs = json.certificates.map(cert => TlsStreamSettings.Cert.fromJson(cert));
        }
        return new TlsStreamSettings(
            json.serverName,
            json.minVersion,
            json.maxVersion,
            json.cipherSuites,
            json.rejectUnknownSni,
            json.alpn,
            json.echServerKeys,
            certs,
            TlsStreamSettings.Settings.fromJson(json.settings),
        );
    }

    toJson() {
        return {
            serverName: this.server,
            minVersion: this.minVersion,
            maxVersion: this.maxVersion,
            cipherSuites: this.cipherSuites,
            rejectUnknownSni: this.rejectUnknownSni,
            alpn: this.alpn,
            echServerKeys: this.echServerKeys,
            certificates: TlsStreamSettings.toJsonArray(this.certs),
            settings: this.settings.toJson(),
        };
    }
}

// settings 是**面板私有**的客户端半边参数，核心的 TLSConfig 里没有这个键。
// 已实测确认核心忽略它而不是拒绝（web/service/inbound_stream_contract_test.go
// 的 TestPanelOnlySettingsKeyIsIgnoredByCore）。存在这里是为了让分享链接
// 能带上 fp / ech 两个参数——它们是客户端要用的，服务端不读。
TlsStreamSettings.Settings = class extends XrayCommonClass {
    constructor(fingerprint='chrome', allowInsecure=false, echConfigList='') {
        super();
        this.fingerprint = fingerprint;
        this.allowInsecure = allowInsecure;
        this.echConfigList = echConfigList;
    }

    static fromJson(json={}) {
        if (ObjectUtil.isEmpty(json)) {
            return new TlsStreamSettings.Settings();
        }
        return new TlsStreamSettings.Settings(
            json.fingerprint,
            json.allowInsecure,
            json.echConfigList,
        );
    }

    toJson() {
        return {
            fingerprint: this.fingerprint,
            allowInsecure: this.allowInsecure,
            echConfigList: this.echConfigList,
        };
    }
};
```

**注意** `fromJson` 里不能对 `json.minVersion` 等做 `|| '1.2'` 的兜底：那会让一个显式存了空串的老入站在编辑时被悄悄改成 1.2。默认值只在构造函数的形参默认值里生效，也就是只对 `undefined` 生效——这正是我们要的语义。

- [ ] **Step 2: 给 Cert 加 ocspStapling**

把 `TlsStreamSettings.Cert`（原 `:452-486`）替换为：

```js
TlsStreamSettings.Cert = class extends XrayCommonClass {
    constructor(useFile=true, certificateFile='', keyFile='', certificate='', key='', ocspStapling=3600) {
        super();
        this.useFile = useFile;
        this.certFile = certificateFile;
        this.keyFile = keyFile;
        this.cert = certificate instanceof Array ? certificate.join('\n') : certificate;
        this.key = key instanceof Array ? key.join('\n') : key;
        this.ocspStapling = ocspStapling;
    }

    static fromJson(json={}) {
        if ('certificateFile' in json && 'keyFile' in json) {
            return new TlsStreamSettings.Cert(
                true,
                json.certificateFile,
                json.keyFile,
                '', '',
                json.ocspStapling,
            );
        } else {
            return new TlsStreamSettings.Cert(
                false, '', '',
                json.certificate.join('\n'),
                json.key.join('\n'),
                json.ocspStapling,
            );
        }
    }

    toJson() {
        if (this.useFile) {
            return {
                certificateFile: this.certFile,
                keyFile: this.keyFile,
                ocspStapling: this.ocspStapling,
            };
        } else {
            return {
                certificate: this.cert.split('\n'),
                key: this.key.split('\n'),
                ocspStapling: this.ocspStapling,
            };
        }
    }
};
```

原来的 `fromJson` 在 `useFile` 分支只传了两个参数，现在多了 `ocspStapling`，中间的 `certificate`/`key` 必须显式补两个空串占位——漏掉会让 `ocspStapling` 落到 `certificate` 形参上。

- [ ] **Step 3: 语法检查**

```bash
node --check web/assets/js/model/xray.js && echo "语法 OK"
```

- [ ] **Step 4: 手工验证 round-trip**

`XUI_DEBUG=true go run main.go`，硬刷新后：

1. 新建一个 vmess + ws + tls 入站，域名填 `example.org`，证书路径随便填两个，保存
2. 重新打开编辑，确认域名与证书路径仍在（`fromJson`/`toJson` 没丢字段）
3. 在浏览器控制台执行，确认生成的 JSON 里有新字段且 `settings` 子对象在位：
   ```js
   JSON.stringify(app.inbounds[0].toJson().stream_settings || '', null, 2)
   ```
   （字段名以页面实际的 Vue 实例为准，找不到时用 Vue DevTools 查看 `inbound.stream.tls.toJson()`）

- [ ] **Step 5: 契约回归与提交**

Task 3 的 `TestPanelOnlySettingsKeyIsIgnoredByCore` 锁的正是这一步生成的形状：

```bash
go test ./web/service/ -run 'TestPanelOnlySettingsKey|TestTlsVisionContract' -v
make verify
git add web/assets/js/model/xray.js
git commit -m "$(cat <<'MSG'
feat(xray.js): TLS 设置补齐 alpn / 版本范围 / cipherSuites / rejectUnknownSni / ECH

面板此前只能配 serverName 和证书路径，核心支持的其余 TLS 参数一个都没暴露。
新增的 settings 子对象存客户端半边（fingerprint / allowInsecure /
echConfigList），只用于生成分享链接，服务端不读——核心忽略这个未知键，
已由 TestPanelOnlySettingsKeyIsIgnoredByCore 实测确认。

fromJson 刻意不做 `|| 默认值` 的兜底：那会让显式存了空串的老入站在编辑
时被悄悄改成新默认值。默认值只对 undefined 生效。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 6: 前端模型——RealityStreamSettings 与 security 三态

**Files:**
- Modify: `web/assets/js/model/xray.js`（在 `TlsStreamSettings.Cert` 之后插入新类；改 `StreamSettings`；改 `Inbound` 的访问器）

**Interfaces:**
- Consumes: Task 4 的 `REALITY_TARGET_PRESETS`、`UTLS_FINGERPRINT`、`Inbound.canEnableReality()`
- Produces: `class RealityStreamSettings` 与 `RealityStreamSettings.Settings`
- Produces: `StreamSettings.reality` 成员、`StreamSettings.isReality` getter/setter
- Produces: `Inbound.reality` getter/setter（与现有 `Inbound.tls` 对称）
- 供 Task 10（REALITY 模板）与 Task 12（分享链接）使用

- [ ] **Step 1: 新增 RealityStreamSettings**

在 `TlsStreamSettings.Cert` 的定义之后、`class StreamSettings` 之前插入：

```js
class RealityStreamSettings extends XrayCommonClass {
    // serverNames 与 shortIds 在这里存成逗号分隔的字符串（表单好填），
    // toJson 时再拆成数组。核心要求两者都非空
    // （infra/conf/transport_security.go:95 与 :136）。
    constructor(show=false,
                xver=0,
                target='',
                serverNames='',
                privateKey='',
                mldsa65Seed='',
                minClientVer='',
                maxClientVer='',
                maxTimeDiff=0,
                shortIds='',
                settings=new RealityStreamSettings.Settings()) {
        super();
        this.show = show;
        this.xver = xver;
        this.target = target;
        this.serverNames = serverNames;
        this.privateKey = privateKey;
        this.mldsa65Seed = mldsa65Seed;
        this.minClientVer = minClientVer;
        this.maxClientVer = maxClientVer;
        this.maxTimeDiff = maxTimeDiff;
        this.shortIds = shortIds;
        this.settings = settings;
    }

    // 拆分逗号分隔串：去空白、去空项、去重且保持首次出现的顺序。
    // 保持顺序而不是排序，是因为项目要求生成逐字节确定——只要规则确定即可，
    // 但绝不能依赖遍历 map 的顺序（见路线图 §4.2）。
    static splitList(value) {
        if (ObjectUtil.isEmpty(value)) {
            return [];
        }
        const seen = new Set();
        const out = [];
        for (const raw of String(value).split(',')) {
            const item = raw.trim();
            if (item === '' || seen.has(item)) {
                continue;
            }
            seen.add(item);
            out.push(item);
        }
        return out;
    }

    static joinList(value) {
        return value instanceof Array ? value.join(',') : (value || '');
    }

    static fromJson(json={}) {
        // dest 与 target 在核心里是别名（transport_security.go:59-61）。
        // 老配置、外部工具和面板早期版本写的都是 dest。不做这个映射的话，
        // 面板读进来 target 为空，用户编辑后一保存就把工作正常的 dest 抹掉，
        // 而且要到下一次重启才暴露。
        const target = ObjectUtil.isEmpty(json.target) ? json.dest : json.target;
        return new RealityStreamSettings(
            json.show,
            json.xver,
            target,
            RealityStreamSettings.joinList(json.serverNames),
            json.privateKey,
            json.mldsa65Seed,
            json.minClientVer,
            json.maxClientVer,
            json.maxTimediff === undefined ? json.maxTimeDiff : json.maxTimediff,
            RealityStreamSettings.joinList(json.shortIds),
            RealityStreamSettings.Settings.fromJson(json.settings),
        );
    }

    toJson() {
        return {
            show: this.show,
            xver: this.xver,
            target: this.target,
            serverNames: RealityStreamSettings.splitList(this.serverNames),
            privateKey: this.privateKey,
            mldsa65Seed: this.mldsa65Seed,
            minClientVer: this.minClientVer,
            maxClientVer: this.maxClientVer,
            maxTimeDiff: this.maxTimeDiff,
            shortIds: RealityStreamSettings.splitList(this.shortIds),
            settings: this.settings.toJson(),
        };
    }
}

// 与 TlsStreamSettings.Settings 同理：面板私有的客户端半边，核心忽略它。
// publicKey 是 x25519 密钥对的公钥，mldsa65Verify 是 ML-DSA-65 的验证公钥，
// 两者都只出现在分享链接里（分别是 pbk 与 pqv 参数）。
RealityStreamSettings.Settings = class extends XrayCommonClass {
    constructor(publicKey='', fingerprint='chrome', serverName='', spiderX='/', mldsa65Verify='') {
        super();
        this.publicKey = publicKey;
        this.fingerprint = fingerprint;
        this.serverName = serverName;
        this.spiderX = spiderX;
        this.mldsa65Verify = mldsa65Verify;
    }

    static fromJson(json={}) {
        if (ObjectUtil.isEmpty(json)) {
            return new RealityStreamSettings.Settings();
        }
        return new RealityStreamSettings.Settings(
            json.publicKey,
            json.fingerprint,
            json.serverName,
            json.spiderX,
            json.mldsa65Verify,
        );
    }

    toJson() {
        return {
            publicKey: this.publicKey,
            fingerprint: this.fingerprint,
            serverName: this.serverName,
            spiderX: this.spiderX,
            mldsa65Verify: this.mldsa65Verify,
        };
    }
};
```

`fromJson` 里 `maxTimediff` 与 `maxTimeDiff` 两种拼写都读：核心的 JSON tag 是 `maxTimeDiff`（`transport_security.go:38`），但 3x-ui 的 schema 用的是 `maxTimediff`。从 3x-ui 导入配置时不兼容会静默丢失这个值。

- [ ] **Step 2: StreamSettings 接入 reality**

`StreamSettings` 构造函数增加 `realitySettings=new RealityStreamSettings()` 形参与 `this.reality = realitySettings;`。

在 `get isTls()` / `set isTls()` 之后（原 `isXTls` 的位置）加入：

```js
    get isReality() {
        return this.security === 'reality';
    }

    set isReality(isReality) {
        this.security = isReality ? 'reality' : 'none';
    }
```

`fromJson` 增加 `RealityStreamSettings.fromJson(json.realitySettings)` 一项；`toJson` 增加：

```js
            realitySettings: this.isReality ? this.reality.toJson() : undefined,
```

- [ ] **Step 3: Inbound 增加 reality 访问器**

在 `Inbound` 的 `set tls()` 之后加入：

```js
    get reality() {
        return this.stream.security === 'reality';
    }

    set reality(isReality) {
        this.stream.security = isReality ? 'reality' : 'none';
    }
```

`get serverName()` 补上 REALITY 分支——REALITY 的 SNI 存在 `reality.serverNames` 的第一项：

```js
    get serverName() {
        if (this.stream.isTls) {
            return this.stream.tls.server;
        }
        if (this.stream.isReality) {
            const names = RealityStreamSettings.splitList(this.stream.reality.serverNames);
            return names.length > 0 ? names[0] : "";
        }
        return "";
    }
```

- [ ] **Step 4: 语法检查**

```bash
node --check web/assets/js/model/xray.js && echo "语法 OK"
```

- [ ] **Step 5: 在浏览器控制台验证 toJson 形状**

`XUI_DEBUG=true go run main.go`，硬刷新后打开入站页，控制台执行：

```js
const s = new StreamSettings();
s.security = 'reality';
s.reality.target = 'www.example.com:443';
s.reality.serverNames = ' a.com , b.com ,, a.com ';
s.reality.shortIds = '0123, , 0123, abcd';
JSON.stringify(s.toJson().realitySettings, null, 2);
```

期望：`serverNames` 为 `["a.com","b.com"]`、`shortIds` 为 `["0123","abcd"]`——去空、去重、保序都生效。

再验证 dest→target 别名：

```js
RealityStreamSettings.fromJson({dest: "x.com:443", serverNames: ["x.com"]}).target;
```

期望：`"x.com:443"`。**这一步返回空字符串的话必须停下修**——它对应设计文档 §4.3 记录的那个「编辑一次就静默抹掉工作中配置」的故障。

- [ ] **Step 6: 契约回归与提交**

```bash
go test ./web/service/ -run 'TestRealityVisionContract' -v
make verify
git add web/assets/js/model/xray.js
git commit -m "$(cat <<'MSG'
feat(xray.js): 新增 REALITY 安全层建模

后端其实早就为 REALITY 铺好了路（xray/hot_diff.go 的 inboundUsesReality
与 util/link 的 reality 解析都在位），只有前端生成不出 realitySettings。

两个容易漏的点写进了代码注释：fromJson 必须做 dest→target 的别名映射，
否则老配置编辑一次就被静默抹掉、要到下次重启才暴露；maxTimediff 与
maxTimeDiff 两种拼写都要读，否则从 3x-ui 导入的配置会静默丢值。

serverNames / shortIds 的拆分保持首次出现顺序而不排序，满足「生成逐字节
确定」的同时不引入新的排序规则。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 7: 前端模型——sniffing destOverride 与存量数据识别

**Files:**
- Modify: `web/assets/js/model/xray.js:572-591`（`Sniffing`）与 `Inbound`（新增 getter）

**Interfaces:**
- Consumes: Task 4 的 `SNIFFING_OPTION`
- Produces: `Inbound.prototype.deprecatedFeatures` getter，返回 `[{field, value, fix}]` 数组，供 Task 11 的警告条与保存按钮禁用逻辑使用

- [ ] **Step 1: Sniffing 保持 destOverride 可编辑**

现有 `Sniffing` 类（`:572-591`）已经有 `destOverride` 字段，`fromJson` 也在处理它——缺的只是模板暴露（Task 12）。这里只需确认 `toJson` 会输出它。`Sniffing` 继承 `XrayCommonClass` 且没有自定义 `toJson`，走的是基类的默认序列化。

在浏览器控制台确认：

```js
new Sniffing().toJson ? new Sniffing().toJson() : JSON.parse(JSON.stringify(new Sniffing()));
```

期望包含 `destOverride: ["http","tls"]`。若基类没有 `toJson` 而是直接被 `JSON.stringify`，同样能拿到该字段——两种情况都可接受，记录实际结果即可。

- [ ] **Step 2: 新增 deprecatedFeatures**

在 `Inbound` 的 `canEnableVision()` 之后加入：

```js
    // 已被当前 Xray 核心移除的配置项。它们仍可能存在于老入站里，
    // 而对应的下拉选项已经从界面上删掉了——如果不显式标出来，用户编辑
    // 这类入站时 a-select 只是显示空白，随手一保存就把传输方式静默改成
    // 别的，这是用户可见行为的静默变更。
    //
    // 返回的每一项都要能直接渲染成一句人话，所以带上 fix。
    get deprecatedFeatures() {
        const found = [];
        if (this.stream.security === 'xtls') {
            found.push({
                field: '安全层',
                value: 'xtls',
                fix: '改用 tls 或 reality。Legacy XTLS 已从核心移除。',
            });
        }
        if (this.stream.network === 'http' || this.stream.network === 'h2') {
            found.push({
                field: '传输方式',
                value: this.stream.network,
                fix: '改用 ws 或 grpc。HTTP/2 传输已从核心移除。',
            });
        }
        if (this.stream.network === 'quic') {
            found.push({
                field: '传输方式',
                value: 'quic',
                fix: '改用 ws 或 grpc。QUIC 传输已从核心移除。',
            });
        }
        const clients = this.settings && (this.settings.vlesses || this.settings.clients);
        if (clients instanceof Array) {
            for (const c of clients) {
                if (c && c.flow && c.flow !== FLOW_CONTROL.VISION) {
                    found.push({
                        field: 'flow',
                        value: c.flow,
                        fix: '改用 xtls-rprx-vision 或留空。',
                    });
                    break;
                }
            }
        }
        return found;
    }
```

注意 `this.settings` 在不同协议下字段名不同（VLESS 是 `vlesses`，TROJAN 是 `clients`），所以两个都试。非这两种协议时 `clients` 为 `undefined`，`instanceof Array` 为 false，安全跳过。

- [ ] **Step 3: 语法检查与控制台验证**

```bash
node --check web/assets/js/model/xray.js && echo "语法 OK"
```

浏览器控制台：

```js
const ib = Inbound.fromJson({
  port: 443, protocol: 'vless',
  settings: JSON.stringify({clients:[{id:'x',flow:'xtls-rprx-direct'}],decryption:'none'}),
  streamSettings: JSON.stringify({network:'http', security:'xtls', tlsSettings:{serverName:''}}),
  sniffing: JSON.stringify({enabled:true,destOverride:['http','tls']}),
});
ib.deprecatedFeatures;
```

期望：返回 3 项（安全层 xtls、传输方式 http、flow xtls-rprx-direct）。

`Inbound.fromJson` 的入参形状以该函数实际签名为准（`web/assets/js/model/xray.js:1047`），上面的字段名若对不上，先读那个函数再调整调用方式——**不要改 fromJson 去迁就这段验证代码**。

- [ ] **Step 4: 提交**

```bash
make verify
git add web/assets/js/model/xray.js
git commit -m "$(cat <<'MSG'
feat(xray.js): 识别已被核心移除的存量配置

把 http/quic 传输、xtls 安全层和旧 flow 值从下拉里删掉之后，老入站会
落进一个更糟的状态：a-select 显示空白，用户随手一保存就把传输方式静默
改成别的。deprecatedFeatures 把这类配置显式列出来，供表单渲染警告条并
禁用保存按钮（Task 11）。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 8: 模板——传输方式下拉与死模板清理

**Files:**
- Modify: `web/html/xui/form/stream/stream_settings.html`
- Delete: `web/html/xui/form/stream/stream_http.html`、`web/html/xui/form/stream/stream_quic.html`
- Modify: `web/web.go`（若有对被删模板的显式引用）

**Interfaces:**
- Consumes: Task 4 删掉的 `HttpStreamSettings` / `QuicStreamSettings`

- [ ] **Step 1: 确认模板加载方式**

```bash
grep -rn 'stream_http\|stream_quic\|streamHTTP\|streamQUIC' web/ --include='*.go' --include='*.html'
```

若 `web/web.go` 是按目录通配加载（`ParseFS` + glob），删文件即可；若有显式列表，同步删除对应条目。把实际结果记下来。

- [ ] **Step 2: 改传输下拉**

把 `web/html/xui/form/stream/stream_settings.html` 里这两行删掉：

```html
            <a-select-option value="http">http</a-select-option>
            <a-select-option value="quic">quic</a-select-option>
```

并删掉文件下方对应的两个渲染块：

```html
<!-- http -->
<template v-if="inbound.stream.network === 'http'">
    {{template "form/streamHTTP"}}
</template>

<!-- quic -->
<template v-if="inbound.stream.network === 'quic'">
    {{template "form/streamQUIC"}}
</template>
```

- [ ] **Step 3: 删掉两个模板文件**

```bash
git rm web/html/xui/form/stream/stream_http.html web/html/xui/form/stream/stream_quic.html
```

- [ ] **Step 4: 跑模板测试**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

期望：PASS。若报 `template "form/streamHTTP" not defined`，说明 Step 2 有遗漏——`getHtmlTemplate` 会吞掉 `ParseFS` 错误，但这个测试不会。

- [ ] **Step 5: 手工验证**

`XUI_DEBUG=true go run main.go`，硬刷新，添加入站 → 传输下拉。确认只剩 tcp / kcp / ws / grpc 四项，逐个切换页面不报错。

- [ ] **Step 6: 提交**

```bash
make verify
git add -A web/html/xui/form/stream/
git commit -m "$(cat <<'MSG'
feat(web): 传输方式下拉移除核心已删的 http 与 quic

这两项选中即导致整份 xray 配置加载失败、全机断网。模型层已在前一个提交
删掉对应的类，这里把界面入口和两个死模板一并清掉。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 9: 模板——TLS 设置区重写

**Files:**
- Modify: `web/html/xui/form/tls_settings.html`（整体重写）

**Interfaces:**
- Consumes: Task 5 的 `TlsStreamSettings` 新字段、Task 4 的 `TLS_VERSION_OPTION` / `TLS_CIPHER_OPTION` / `UTLS_FINGERPRINT` / `ALPN_OPTION`
- Consumes: Task 2 的 `POST server/getNewEchCert`

- [ ] **Step 1: 重写模板**

把 `web/html/xui/form/tls_settings.html` 整体替换为：

```html
{{define "form/tlsSettings"}}
<!-- tls settings（安全层的三态选择器在 inbound.html，这里只管 tls 的参数） -->
<a-form v-if="inbound.stream.isTls" layout="inline">
    <a-form-item label="域名">
        <a-input v-model.trim="inbound.stream.tls.server"></a-input>
    </a-form-item>
    <a-form-item label="证书">
        <a-radio-group v-model="inbound.stream.tls.certs[0].useFile"
                       button-style="solid">
            <a-radio-button :value="true">证书文件路径</a-radio-button>
            <a-radio-button :value="false">证书文件内容</a-radio-button>
        </a-radio-group>
    </a-form-item>
    <template v-if="inbound.stream.tls.certs[0].useFile">
        <a-form-item label="公钥文件路径">
            <a-input v-model.trim="inbound.stream.tls.certs[0].certFile"></a-input>
        </a-form-item>
        <a-form-item label="密钥文件路径">
            <a-input v-model.trim="inbound.stream.tls.certs[0].keyFile"></a-input>
        </a-form-item>
    </template>
    <template v-else>
        <a-form-item label="公钥内容">
            <a-input type="textarea" :rows="2"
                     v-model="inbound.stream.tls.certs[0].cert"></a-input>
        </a-form-item>
        <a-form-item label="密钥内容">
            <a-input type="textarea" :rows="2"
                     v-model="inbound.stream.tls.certs[0].key"></a-input>
        </a-form-item>
    </template>

    <a-form-item>
        <span slot="label">
            alpn
            <a-tooltip>
                <template slot="title">
                    TLS 握手时协商的应用层协议。保持默认即可。
                </template>
                <a-icon type="question-circle" theme="filled"></a-icon>
            </a-tooltip>
        </span>
        <a-select mode="multiple" v-model="inbound.stream.tls.alpn" style="width: 220px;">
            <a-select-option v-for="a in ALPN_OPTION" :key="a" :value="a">[[ a ]]</a-select-option>
        </a-select>
    </a-form-item>

    <a-form-item>
        <span slot="label">
            客户端指纹
            <a-tooltip>
                <template slot="title">
                    写进分享链接的 fp 参数，让客户端模仿对应浏览器的 TLS 指纹。<br/>
                    这个值服务端不读，填错了服务端一切正常，是客户端连不上。
                </template>
                <a-icon type="question-circle" theme="filled"></a-icon>
            </a-tooltip>
        </span>
        <a-select v-model="inbound.stream.tls.settings.fingerprint" style="width: 150px;">
            <a-select-option v-for="f in UTLS_FINGERPRINT" :key="f" :value="f">[[ f ]]</a-select-option>
        </a-select>
    </a-form-item>
</a-form>

<!-- tls 高级设置 -->
<a-collapse v-if="inbound.stream.isTls" style="margin-top: 8px;">
    <a-collapse-panel header="TLS 高级设置（留空即安全默认）">
        <a-form layout="inline">
            <a-form-item>
                <span slot="label">
                    最低版本
                    <a-tooltip>
                        <template slot="title">
                            使用 xtls-rprx-vision 时必须是 1.3：核心在运行期检查外层 TLS 版本，<br/>
                            低于 1.3 会直接断开，而配置校验发现不了。
                        </template>
                        <a-icon type="question-circle" theme="filled"></a-icon>
                    </a-tooltip>
                </span>
                <a-select v-model="inbound.stream.tls.minVersion"
                          :disabled="inbound.visionEnabled" style="width: 100px;">
                    <a-select-option v-for="v in TLS_VERSION_OPTION" :key="v" :value="v">[[ v ]]</a-select-option>
                </a-select>
            </a-form-item>
            <a-form-item label="最高版本">
                <a-select v-model="inbound.stream.tls.maxVersion" style="width: 100px;">
                    <a-select-option v-for="v in TLS_VERSION_OPTION" :key="v" :value="v">[[ v ]]</a-select-option>
                </a-select>
            </a-form-item>
            <a-form-item>
                <span slot="label">
                    cipherSuites
                    <a-tooltip>
                        <template slot="title">
                            <b>仅影响 TLS 1.2 握手。</b>Vision 与 REALITY 走 TLS 1.3，此项不生效。<br/>
                            留空表示使用 Go 的默认套件，通常就是最好的选择。
                        </template>
                        <a-icon type="question-circle" theme="filled"></a-icon>
                    </a-tooltip>
                </span>
                <a-select mode="multiple" :value="tlsCipherList"
                          @change="onTlsCipherChange" style="width: 420px;"
                          placeholder="留空 = auto">
                    <a-select-option v-for="c in TLS_CIPHER_OPTION" :key="c" :value="c">[[ c ]]</a-select-option>
                </a-select>
            </a-form-item>
            <a-form-item>
                <span slot="label">
                    rejectUnknownSni
                    <a-tooltip>
                        <template slot="title">
                            开启后，SNI 与证书域名不匹配的连接直接拒绝。<br/>
                            能挡掉一部分主动探测，但域名填错时自己也连不上。
                        </template>
                        <a-icon type="question-circle" theme="filled"></a-icon>
                    </a-tooltip>
                </span>
                <a-switch v-model="inbound.stream.tls.rejectUnknownSni"></a-switch>
            </a-form-item>
            <a-form-item label="ocspStapling">
                <a-input-number v-model="inbound.stream.tls.certs[0].ocspStapling"
                                :min="0"></a-input-number>
            </a-form-item>
        </a-form>

        <a-form layout="inline" style="margin-top: 8px;">
            <a-form-item>
                <span slot="label">
                    ECH
                    <a-tooltip>
                        <template slot="title">
                            Encrypted Client Hello：把 TLS 握手里的 SNI 也加密，<br/>
                            外部观察者只看得到 publicName。<br/>
                            <b>需要客户端支持</b>：不支持 ECH 的客户端会静默按普通 TLS 连接，<br/>
                            不报错，但也没有这层保护。
                        </template>
                        <a-icon type="question-circle" theme="filled"></a-icon>
                    </a-tooltip>
                </span>
                <a-button type="primary" size="small" :loading="echLoading"
                          @click="genEchCert">生成 ECH 密钥</a-button>
            </a-form-item>
            <a-form-item label="echServerKeys">
                <a-input v-model.trim="inbound.stream.tls.echServerKeys"
                         style="width: 320px;"></a-input>
            </a-form-item>
            <a-form-item label="echConfigList">
                <a-input v-model.trim="inbound.stream.tls.settings.echConfigList"
                         style="width: 320px;"></a-input>
            </a-form-item>
        </a-form>
    </a-collapse-panel>
</a-collapse>
{{end}}
```

- [ ] **Step 2: 在入站弹窗的 Vue 实例上补齐这个模板用到的成员**

上面的模板引用了 `tlsCipherList`、`onTlsCipherChange`、`echLoading`、`genEchCert`、`inbound.visionEnabled`。

先找到入站弹窗的 Vue 实例：

```bash
grep -rn 'new Vue' web/html/xui/inbound_modal.html web/html/xui/inbounds.html
```

`echLoading` 已在 Task 4 Step 6 加进 `data`，这里不必再动 `data`。

在该实例的 `computed` 中加（现有实例没有 `computed` 块时，加在 `data` 与 `methods` 之间）：

```js
            tlsCipherList() {
                const raw = this.inbound.stream.tls.cipherSuites;
                return raw ? raw.split(':').filter(s => s !== '') : [];
            },
```

`methods` 中加：

```js
            onTlsCipherChange(list) {
                // 核心按 ":" 切分（transport/internet/tls/config.go:459），
                // 且对查不到的名字静默丢弃，所以这里只允许从固定列表里选。
                this.inbound.stream.tls.cipherSuites = list.join(':');
            },
            async genEchCert() {
                this.echLoading = true;
                try {
                    const msg = await HttpUtil.post('/server/getNewEchCert', {
                        serverName: this.inbound.stream.tls.server,
                    });
                    if (!msg.success) {
                        return;
                    }
                    this.inbound.stream.tls.echServerKeys = msg.obj.echServerKeys;
                    this.inbound.stream.tls.settings.echConfigList = msg.obj.echConfigList;
                } finally {
                    this.echLoading = false;
                }
            },
```

`HttpUtil` 的接口是 `static async post(url, data, options)`（`web/assets/js/util/utils.js:43`），返回一个 `Msg`（`{success, msg, obj}`），并且**已经在内部弹好了成功/失败提示**（`_handleMsg`），所以上面的代码只判断 `msg.success` 而不再自己弹消息。

URL 用带前导斜杠的绝对路径 `'/server/getNewEchCert'`，与仓库既有写法一致（`web/html/xui/index.html:288` 的 `'/server/status'`、`web/html/xui/inbounds.html:390` 的 `'/xui/inbound/list'`）。**注意**：这个写法在自定义 basePath 部署下会失效，但那是全仓库既有的问题，本次不顺手修——保持一致比单点正确重要。

`inbound.visionEnabled` 在 Task 11 与 flow 选择器一起加；本任务先在 `Inbound` 上加一个最小实现，避免模板引用未定义成员：

在 `web/assets/js/model/xray.js` 的 `Inbound` 中加：

```js
    // 当前是否真的启用了 Vision。TLS 路径下它会把 minVersion 锁死为 1.3。
    get visionEnabled() {
        const clients = this.settings && (this.settings.vlesses || this.settings.clients);
        if (!(clients instanceof Array) || clients.length === 0) {
            return false;
        }
        return clients[0].flow === FLOW_CONTROL.VISION;
    }
```

- [ ] **Step 3: 跑模板测试**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

期望：PASS。`TestVueDirectivesLiveInsideAVueRoot` 尤其重要——新加的 `a-collapse` 若落在 `#app` 之外，页面照常渲染但所有绑定失效且控制台无报错。

- [ ] **Step 4: 手工验证**

`XUI_DEBUG=true go run main.go`，硬刷新。新建 vmess + ws 入站，开启 tls：

1. 域名、证书、alpn、客户端指纹都能填能选
2. 展开「TLS 高级设置」，cipherSuites 多选后收起再展开，选中项还在（`tlsCipherList` 的双向转换正确）
3. 点「生成 ECH 密钥」，两个输入框被填上 base64 串
4. 保存后重新编辑，所有字段值都还在

- [ ] **Step 5: 提交**

```bash
make verify
git add web/html/xui/form/tls_settings.html web/assets/js/model/xray.js web/html/xui/inbound_modal.html
git commit -m "$(cat <<'MSG'
feat(web): TLS 设置区重写，补齐 alpn/版本/cipherSuites/ECH

按「基础项直接可见、高级项收进折叠区、留空即安全默认」组织，避免把
二十个参数摊在非专业管理员面前。

cipherSuites 做成固定列表的多选而不是文本框，因为核心对查不到的套件名
静默丢弃（config.go:459-463 没有 else 分支），拼错一个字符不会有任何
反馈。提示里也写明它对 TLS 1.3 无效——Vision 与 REALITY 都走 1.3。

ECH 的提示写明「不支持的客户端会静默降级」，避免管理员误以为开了就
一定生效。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 10: 模板——REALITY 表单

**Files:**
- Create: `web/html/xui/form/reality_settings.html`
- Modify: 入站弹窗的 Vue 实例（`methods` 增加 `genRealityKeys`、`genShortId`、`genMldsa65`）

**Interfaces:**
- Consumes: Task 6 的 `RealityStreamSettings`、Task 4 的 `REALITY_TARGET_PRESETS` / `UTLS_FINGERPRINT`
- Consumes: Task 1 的 `POST server/getNewX25519Cert` 与 `POST server/getNewMldsa65`

- [ ] **Step 1: 新建模板**

创建 `web/html/xui/form/reality_settings.html`：

```html
{{define "form/realitySettings"}}
<a-form v-if="inbound.stream.isReality" layout="inline">
    <a-form-item>
        <span slot="label">
            伪装目标
            <a-tooltip>
                <template slot="title">
                    REALITY 会借用这个真实网站的 TLS 握手。GFW 主动探测你的端口时，<br/>
                    看到的是这个站点的真实响应。<br/>
                    <b>不要选 .cn/.ru/.ir 域名，也不要选 apple/icloud/microsoft 相关域名</b>——<br/>
                    Xray 核心明确警告这几类会提高 IP 被封的概率。
                </template>
                <a-icon type="question-circle" theme="filled"></a-icon>
            </a-tooltip>
        </span>
        <!-- combobox 模式：既能从预置列表里选，也能直接输入自定义目标。
             ant-design-vue 1.7.2 用 mode="combobox" 而不是 4.x 的 allowClear+search。 -->
        <a-select mode="combobox" v-model="inbound.stream.reality.target"
                  style="width: 280px;" placeholder="域名:443"
                  :filter-option="false">
            <a-select-option v-for="t in REALITY_TARGET_PRESETS" :key="t" :value="t + ':443'">[[ t ]]:443</a-select-option>
        </a-select>
    </a-form-item>

    <a-form-item v-if="realityTargetRisky">
        <a-alert type="warning" show-icon
                 message="这个伪装目标会提高 IP 被封锁的概率"
                 description="Xray 核心对 .cn / .ru / .ir 后缀，以及含 apple / icloud / microsoft 的域名会发出该警告。核心只把它写进启动日志，面板不展示，所以在这里主动提示。建议换一个预置目标。">
        </a-alert>
    </a-form-item>
    <a-form-item>
        <span slot="label">
            serverNames
            <a-tooltip>
                <template slot="title">
                    客户端握手时使用的 SNI，逗号分隔可填多个。<br/>
                    通常就是伪装目标的域名。
                </template>
                <a-icon type="question-circle" theme="filled"></a-icon>
            </a-tooltip>
        </span>
        <a-input v-model.trim="inbound.stream.reality.serverNames" style="width: 280px;"></a-input>
    </a-form-item>

    <a-form-item v-if="inbound.port !== 443">
        <a-alert type="warning" show-icon
                 message="REALITY 建议监听 443 端口"
                 description="Xray 核心明确警告：监听非 443 端口会提高服务器 IP 被 GFW 封锁的概率。这条警告只出现在核心的启动日志里，面板不展示，所以这里主动提示。">
        </a-alert>
    </a-form-item>

    <a-form-item label="密钥">
        <a-button type="primary" size="small" :loading="realityLoading"
                  @click="genRealityKeys">重新生成密钥对</a-button>
    </a-form-item>
    <a-form-item label="privateKey">
        <a-input v-model.trim="inbound.stream.reality.privateKey" style="width: 320px;"></a-input>
    </a-form-item>
    <a-form-item label="publicKey">
        <a-input v-model.trim="inbound.stream.reality.settings.publicKey" style="width: 320px;"></a-input>
    </a-form-item>
    <a-form-item>
        <span slot="label">
            shortIds
            <a-tooltip>
                <template slot="title">
                    十六进制字符，单个不超过 16 位，逗号分隔可填多个。
                </template>
                <a-icon type="question-circle" theme="filled"></a-icon>
            </a-tooltip>
        </span>
        <a-input v-model.trim="inbound.stream.reality.shortIds" style="width: 240px;"></a-input>
    </a-form-item>
    <a-form-item label=" ">
        <a-button size="small" @click="genShortId">随机</a-button>
    </a-form-item>
    <a-form-item label="客户端指纹">
        <a-select v-model="inbound.stream.reality.settings.fingerprint" style="width: 150px;">
            <a-select-option v-for="f in UTLS_FINGERPRINT" :key="f" :value="f">[[ f ]]</a-select-option>
        </a-select>
    </a-form-item>
    <a-form-item label="spiderX">
        <a-input v-model.trim="inbound.stream.reality.settings.spiderX" style="width: 120px;"></a-input>
    </a-form-item>
</a-form>

<a-collapse v-if="inbound.stream.isReality" style="margin-top: 8px;">
    <a-collapse-panel header="REALITY 高级设置（留空即安全默认）">
        <a-form layout="inline">
            <a-form-item>
                <span slot="label">
                    minClientVer
                    <a-tooltip>
                        <template slot="title">
                            <b style="color:#cf1322">留空最安全。</b>核心默认只接受 Xray v26.3.27 及以上的客户端。<br/>
                            核心源码里写明：调低这个值会提高服务器 IP 被 GFW 封锁的概率。<br/>
                            旧客户端连不上时，正确做法是升级客户端，而不是调低这里。
                        </template>
                        <a-icon type="question-circle" theme="filled"></a-icon>
                    </a-tooltip>
                </span>
                <a-input v-model.trim="inbound.stream.reality.minClientVer"
                         placeholder="留空 = 26.3.27" style="width: 160px;"></a-input>
            </a-form-item>
            <a-form-item label="maxClientVer">
                <a-input v-model.trim="inbound.stream.reality.maxClientVer" style="width: 160px;"></a-input>
            </a-form-item>
            <a-form-item label="maxTimeDiff">
                <a-input-number v-model="inbound.stream.reality.maxTimeDiff" :min="0"></a-input-number>
            </a-form-item>
            <a-form-item>
                <span slot="label">
                    xver
                    <a-tooltip>
                        <template slot="title">PROXY protocol 版本，只接受 0 / 1 / 2。默认 0。</template>
                        <a-icon type="question-circle" theme="filled"></a-icon>
                    </a-tooltip>
                </span>
                <a-input-number v-model="inbound.stream.reality.xver" :min="0" :max="2"></a-input-number>
            </a-form-item>
            <a-form-item label="show">
                <a-switch v-model="inbound.stream.reality.show"></a-switch>
            </a-form-item>
        </a-form>

        <a-form layout="inline" style="margin-top: 8px;">
            <a-form-item>
                <span slot="label">
                    ML-DSA-65
                    <a-tooltip>
                        <template slot="title">
                            REALITY 的后量子签名。留空表示不启用。<br/>
                            <b>需要客户端支持</b>，旧客户端会连不上。
                        </template>
                        <a-icon type="question-circle" theme="filled"></a-icon>
                    </a-tooltip>
                </span>
                <a-button size="small" :loading="mldsaLoading" @click="genMldsa65">生成后量子密钥</a-button>
            </a-form-item>
            <a-form-item label="mldsa65Seed">
                <a-input v-model.trim="inbound.stream.reality.mldsa65Seed" style="width: 320px;"></a-input>
            </a-form-item>
            <a-form-item label="mldsa65Verify">
                <a-input v-model.trim="inbound.stream.reality.settings.mldsa65Verify" style="width: 320px;"></a-input>
            </a-form-item>
        </a-form>
    </a-collapse-panel>
</a-collapse>
{{end}}
```

- [ ] **Step 2: 在 stream_settings.html 里引入这个模板**

在 `web/html/xui/form/stream/stream_settings.html` 末尾、`{{end}}` 之前加：

```html
<!-- reality -->
{{template "form/realitySettings"}}
```

REALITY 的表单不按 network 分支渲染（它自己的 `v-if="inbound.stream.isReality"` 已经管住了），放在传输方式块之后即可。

- [ ] **Step 3: 补 Vue 实例的方法**

`realityLoading` 与 `mldsaLoading` 已在 Task 4 Step 6 加进 `data`。

`computed` 加高风险目标判定——判定逻辑逐字对应核心的 `transport_security.go:164-170`：

```js
            // 与核心 transport_security.go:163-169 的判定逐条对应。
            //
            // 【本计划初稿在这里写错过】核心那条日志的措辞是
            // `Choosing "<sn>" as the target will increase...`，看着像在说
            // 「伪装目标」，但它遍历的切片是 config.ServerNames。判定的是
            // serverNames，与 target/dest 无关。按 target 判会在「target 安全、
            // serverNames 危险」时漏报——恰好是本功能要消灭的那类静默失效。
            //
            // 核心命中时只在启动日志里 LogWarning，配置照常加载，而面板从不
            // 展示 xray 日志——不在这里提示，管理员没有任何途径知道。
            realityTargetRisky() {
                const raw = this.inbound.stream.reality.serverNames || '';
                // serverNames 存的是逗号分隔的多个域名，任一命中即为风险。
                for (const item of String(raw).split(',')) {
                    const sn = item.trim().toLowerCase();
                    if (sn === '') {
                        continue;
                    }
                    if (sn.endsWith('.ru') || sn.endsWith('.ir') || sn.endsWith('.cn')) {
                        return true;
                    }
                    if (sn.includes('apple') || sn.includes('icloud') || sn.includes('microsoft')) {
                        return true;
                    }
                }
                return false;
            },
```

`methods` 加：

```js
            async genRealityKeys() {
                this.realityLoading = true;
                try {
                    const msg = await HttpUtil.post('/server/getNewX25519Cert');
                    if (!msg.success) {
                        return;
                    }
                    this.inbound.stream.reality.privateKey = msg.obj.privateKey;
                    this.inbound.stream.reality.settings.publicKey = msg.obj.publicKey;
                } finally {
                    this.realityLoading = false;
                }
            },
            genShortId() {
                // 核心要求 shortId 是十六进制且不超过 16 位
                // （transport_security.go:140-145 走 hex.Decode）。
                const hex = '0123456789abcdef';
                let out = '';
                for (let i = 0; i < 16; i++) {
                    out += hex[Math.floor(Math.random() * 16)];
                }
                this.inbound.stream.reality.shortIds = out;
            },
            async genMldsa65() {
                this.mldsaLoading = true;
                try {
                    const msg = await HttpUtil.post('/server/getNewMldsa65');
                    if (!msg.success) {
                        return;
                    }
                    this.inbound.stream.reality.mldsa65Seed = msg.obj.seed;
                    this.inbound.stream.reality.settings.mldsa65Verify = msg.obj.verify;
                } finally {
                    this.mldsaLoading = false;
                }
            },
```

- [ ] **Step 4: 跑模板测试**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

期望：PASS。

- [ ] **Step 5: 手工验证（这一步是本计划最关键的一次端到端验证）**

`XUI_DEBUG=true go run main.go`，硬刷新，添加入站：

1. 协议选 `vless`，传输选 `tcp`，安全层选 `reality`（三态选择器在 Task 11，此时可先在控制台执行 `app.inbound.stream.security = 'reality'` 触发渲染）
2. 端口保持随机值 → 确认出现「建议监听 443 端口」的黄色警告
3. 端口改成 443 → 警告消失
4. 伪装目标下拉里能选到 Task 4 复核确认的五个域名
5. 手工输入 `www.microsoft.com:443` → 出现「这个伪装目标会提高 IP 被封锁的概率」警告；再输入 `www.taobao.cn:443` → 同样出现（后缀判定）；换回预置域名 → 警告消失
6. 点「重新生成密钥对」→ privateKey 与 publicKey 都被填上
7. 点 shortIds 旁的「随机」→ 填上 16 位十六进制
8. 展开高级设置，点「生成后量子密钥」→ seed 与 verify 都被填上
9. **保存，确认保存成功**（这一步会真正走 `ValidateInboundReplacing`，用真实 xray 校验整份配置）
10. 重新打开编辑，确认所有字段值都还在

第 9 步失败时，错误信息会带上 xray 的原话，据此定位是哪个字段的格式不对。

- [ ] **Step 6: 提交**

```bash
make verify
git add web/html/xui/form/reality_settings.html web/html/xui/form/stream/stream_settings.html web/html/xui/inbound_modal.html
git commit -m "$(cat <<'MSG'
feat(web): 新增 REALITY 入站表单

REALITY 不需要自有域名和证书，握手时借用真实大站的证书链，主动探测会
看到那个站点的真实响应——对「防止 IP 被墙」比自有域名 + TLS 更强，因为
没有可被关联的域名。

三处提示直接引用核心源码里的风险判定，这些警告核心只写在启动日志里，
而面板从不展示 xray 日志，管理员实际上没有任何途径看到：
- 非 443 端口会提高被封概率（infra/conf/xray.go:177-179）
- minClientVer 调低会提高被封概率（transport_security.go:116）
- .cn/.ru/.ir 与 apple/icloud/microsoft 类伪装目标同理（:164-170）

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 11: 模板——安全层三态选择器、flow、sniffing、存量警告条

**Files:**
- Modify: `web/html/xui/form/inbound.html`
- Modify: `web/html/xui/form/sniffing.html`
- Modify: `web/html/xui/form/protocol/vless.html`
- Modify: `web/html/xui/form/protocol/vmess.html`、`trojan.html`、`shadowsocks.html`（删残留的 xtls 开关）
- Modify: 入站弹窗的保存按钮

**Interfaces:**
- Consumes: Task 4 的 `canEnableTls()` / `canEnableReality()` / `canEnableVision()`、Task 7 的 `deprecatedFeatures`、Task 4 的 `SNIFFING_OPTION`

- [ ] **Step 1: 存量警告条**

在 `web/html/xui/form/inbound.html` 的 `{{define "form/inbound"}}` 之后、第一个 `<a-form>` 之前插入：

```html
<!-- 已被当前 Xray 核心移除的配置项。不显式标出来的话，对应下拉会显示空白，
     用户随手一保存就把配置静默改成别的。 -->
<a-alert v-if="inbound.deprecatedFeatures.length > 0" type="error" show-icon
         style="margin-bottom: 12px;"
         message="此入站使用了当前 Xray 核心已移除的配置，必须修改后才能保存">
    <template slot="description">
        <div v-for="d in inbound.deprecatedFeatures" :key="d.field + d.value">
            [[ d.field ]]：<b>[[ d.value ]]</b> —— [[ d.fix ]]
        </div>
    </template>
</a-alert>
```

- [ ] **Step 2: 安全层三态选择器**

在 `inbound.html` 的传输/协议块之后（`{{template "form/streamSettings"}}` 所在位置的前面）加入：

```html
<!-- 安全层 -->
<a-form v-if="inbound.canEnableTls()" layout="inline">
    <a-form-item>
        <span slot="label">
            安全层
            <a-tooltip>
                <template slot="title">
                    reality 抗封锁最强且不需要自有域名；tls 需要域名和证书。<br/>
                    reality 只支持 tcp 与 grpc 传输。
                </template>
                <a-icon type="question-circle" theme="filled"></a-icon>
            </a-tooltip>
        </span>
        <a-radio-group v-model="inbound.stream.security" button-style="solid"
                       @change="onSecurityChange">
            <a-radio-button value="none">none</a-radio-button>
            <a-radio-button value="tls">tls</a-radio-button>
            <a-radio-button value="reality" :disabled="!inbound.canEnableReality()">reality</a-radio-button>
        </a-radio-group>
    </a-form-item>
</a-form>
```

在 Vue 实例的 `methods` 加：

```js
            onSecurityChange(e) {
                const security = e.target ? e.target.value : e;
                if (security !== 'reality') {
                    return;
                }
                // 选中 REALITY 时给一套安全默认：端口 443（核心明确警告非 443
                // 会提高被封概率）、自动生成密钥与 shortId、flow 置 Vision。
                if (this.inbound.port !== 443) {
                    this.inbound.port = 443;
                }
                if (!this.inbound.stream.reality.privateKey) {
                    this.genRealityKeys();
                }
                if (!this.inbound.stream.reality.shortIds) {
                    this.genShortId();
                }
                if (this.inbound.canEnableVision()) {
                    this.setFlow(FLOW_CONTROL.VISION);
                }
            },
            setFlow(flow) {
                const clients = this.inbound.settings
                    && (this.inbound.settings.vlesses || this.inbound.settings.clients);
                if (clients instanceof Array && clients.length > 0) {
                    this.$set(clients[0], 'flow', flow);
                }
            },
```

- [ ] **Step 3: vless 的 flow 下拉**

把 `web/html/xui/form/protocol/vless.html:6-10` 那个 `v-if="inbound.xtls"` 的块替换为：

```html
    <a-form-item v-if="inbound.canEnableVision()">
        <span slot="label">
            flow
            <a-tooltip>
                <template slot="title">
                    xtls-rprx-vision 能显著削弱流量特征，是当前核心唯一支持的 flow。<br/>
                    使用它时外层 TLS 必须是 1.3，选中后最低版本会自动锁定。<br/>
                    注意它不支持 UDP 请求。
                </template>
                <a-icon type="question-circle" theme="filled"></a-icon>
            </a-tooltip>
        </span>
        <a-select :value="inbound.settings.vlesses[0].flow"
                  @change="onFlowChange" style="width: 200px">
            <a-select-option value="">（不使用）</a-select-option>
            <a-select-option :value="FLOW_CONTROL.VISION">[[ FLOW_CONTROL.VISION ]]</a-select-option>
        </a-select>
    </a-form-item>
```

Vue 实例 `methods` 加：

```js
            onFlowChange(flow) {
                this.setFlow(flow);
                // Vision 要求外层 TLS 1.3：核心在运行期才检查
                // （proxy/vless/inbound/inbound.go:573），配置校验发现不了，
                // 表现为「保存成功但客户端连不上」。所以在这里锁死。
                if (flow === FLOW_CONTROL.VISION && this.inbound.stream.security === 'tls') {
                    this.inbound.stream.tls.minVersion = '1.3';
                    this.inbound.stream.tls.maxVersion = '1.3';
                }
            },
```

- [ ] **Step 4: 删掉各协议表单里残留的 xtls 开关**

```bash
grep -rn 'xtls' web/html/xui/form/
```

逐个删除找到的 `inbound.xtls` / `canEnableXTls` 引用。**必须删干净**：Task 4 已经把 `Inbound.xtls` 这个 setter 删了，残留的 `v-model="inbound.xtls"` 会在渲染时报错。

- [ ] **Step 5: sniffing 的 destOverride**

把 `web/html/xui/form/sniffing.html` 整体替换为：

```html
{{define "form/sniffing"}}
<a-form layout="inline">
  <a-form-item>
            <span slot="label">
                sniffing
                <a-tooltip>
                    <template slot="title">
                        嗅探出目标域名，域名分流规则依赖它。<br/>
                        <b>关掉 sniffing，或 destOverride 不含 http/tls，域名分流规则就永远不会命中，而且没有任何报错。</b>
                    </template>
                    <a-icon type="question-circle" theme="filled"></a-icon>
                </a-tooltip>
            </span>
    <a-switch v-model="inbound.sniffing.enabled"></a-switch>
  </a-form-item>
  <a-form-item v-if="inbound.sniffing.enabled">
    <a-checkbox-group v-model="inbound.sniffing.destOverride" :options="SNIFFING_OPTION"></a-checkbox-group>
  </a-form-item>
</a-form>
{{end}}
```

- [ ] **Step 6: 保存按钮在有存量警告时禁用**

入站弹窗是 `web/html/xui/inbound_modal.html:2-6` 的 `<a-modal id="inbound-modal">`，用的是 antd 内置 footer（`:ok-text="inModal.okText"` + `@ok="inModal.ok"`），没有自定义 footer。ant-design-vue 1.7.2 用 `ok-button-props` 控制确定按钮：

```html
<a-modal id="inbound-modal" v-model="inModal.visible" :title="inModal.title" @ok="inModal.ok"
         :confirm-loading="inModal.confirmLoading" :closable="true" :mask-closable="false"
         :ok-text="inModal.okText" cancel-text='{{ i18n "close" }}'
         :ok-button-props="{ props: { disabled: inbound.deprecatedFeatures.length > 0 } }">
    {{template "form/inbound"}}
</a-modal>
```

注意 1.7.2 的 `ok-button-props` 要求 `{ props: { ... } }` 这一层嵌套，直接写 `{ disabled: ... }` 不生效且不报错。

- [ ] **Step 7: 模板测试**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

期望：PASS。

- [ ] **Step 8: 手工验证**

`XUI_DEBUG=true go run main.go`，硬刷新：

1. 新建 vless + tcp → 安全层三个按钮都在，reality 可选
2. 传输改成 ws → reality 按钮变灰（`canEnableReality()` 只允许 tcp/grpc）
3. 传输改回 tcp，点 reality → 端口自动变 443、密钥与 shortId 自动填上、flow 自动变成 xtls-rprx-vision
4. 安全层改成 tls，flow 保持 vision → 展开 TLS 高级设置，确认最低版本已锁死 1.3 且不可改
5. sniffing 开关下方出现 http/tls/quic/fakedns 四个复选框，默认勾选 http 与 tls
6. 保存成功
7. **存量数据验证**：用 sqlite 手工造一条 `security=xtls` 的入站，然后在面板里编辑它：
   ```bash
   sqlite3 /etc/a-ui/a-ui.db "UPDATE inbounds SET stream_settings='{\"network\":\"tcp\",\"security\":\"xtls\",\"xtlsSettings\":{\"serverName\":\"a.com\"}}' WHERE id=<某个测试入站的 id>;"
   ```
   确认：红色警告条出现、列出「安全层：xtls」、保存按钮变灰。**改完记得把这条测试数据删掉或改回去。**

- [ ] **Step 9: 提交**

```bash
make verify
git status
git add web/html/xui/form/ web/html/xui/inbound_modal.html web/assets/js/model/xray.js
git commit -m "$(cat <<'MSG'
feat(web): 安全层三态选择器、Vision flow、sniffing destOverride 与存量配置警告

安全层从「各协议表单里散落的 tls/xtls 开关」改为统一的 none/tls/reality
三态，并按核心的真实约束联动禁用：reality 只支持 tcp 与 grpc，Vision 只
对 vless 有效且会把 TLS 锁死在 1.3。

最后这条尤其重要：Vision 对 TLS 1.3 的要求是核心在运行期才检查的
（proxy/vless/inbound/inbound.go:573），xray run -test 过得去、入站保存
成功，然后客户端连不上——后端校验对这类故障无能为力，只能在表单挡住。

sniffing 的 destOverride 此前完全没有界面入口，而域名分流硬依赖它，
关掉之后规则永远不命中且没有任何报错。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 12: 分享链接生成

**Files:**
- Modify: `web/assets/js/model/xray.js:946-1017`（`genVLESSLink`）、`:882-945`（`genVmessLink`）、`:1032-1036`（`genTrojanLink`）

**Interfaces:**
- Consumes: Task 5 的 `TlsStreamSettings.settings`、Task 6 的 `RealityStreamSettings.settings`
- 参数名必须与 `util/link/outbound.go` 的解析端一致：`pbk`/`sid`/`sni`/`fp`/`spx`/`pqv`/`ech`/`alpn`/`flow`

- [ ] **Step 1: 改 genVLESSLink**

把 `genVLESSLink` 中 `params.set("security", ...)` 的那段（原 `:953-958`）替换为：

```js
        params.set("security", this.stream.security);
```

删掉 `case "http":` 与 `case "quic":` 两个分支（对应的类已在 Task 4 删除）。

把原来 `:1000-1013` 的 tls / xtls 段替换为：

```js
        if (this.stream.security === 'tls') {
            const tls = this.stream.tls;
            if (!ObjectUtil.isEmpty(tls.server)) {
                address = tls.server;
                params.set("sni", tls.server);
            }
            if (tls.alpn instanceof Array && tls.alpn.length > 0) {
                params.set("alpn", tls.alpn.join(','));
            }
            if (!ObjectUtil.isEmpty(tls.settings.fingerprint)) {
                params.set("fp", tls.settings.fingerprint);
            }
            // util/link/outbound.go:461 与 :675 把 URI 的 ech 参数映射到
            // echConfigList，两端必须用同一个名字。
            if (!ObjectUtil.isEmpty(tls.settings.echConfigList)) {
                params.set("ech", tls.settings.echConfigList);
            }
        } else if (this.stream.security === 'reality') {
            const re = this.stream.reality;
            const names = RealityStreamSettings.splitList(re.serverNames);
            if (names.length > 0) {
                params.set("sni", names[0]);
            }
            params.set("pbk", re.settings.publicKey);
            const ids = RealityStreamSettings.splitList(re.shortIds);
            if (ids.length > 0) {
                params.set("sid", ids[0]);
            }
            if (!ObjectUtil.isEmpty(re.settings.fingerprint)) {
                params.set("fp", re.settings.fingerprint);
            }
            if (!ObjectUtil.isEmpty(re.settings.spiderX)) {
                params.set("spx", re.settings.spiderX);
            }
            if (!ObjectUtil.isEmpty(re.settings.mldsa65Verify)) {
                params.set("pqv", re.settings.mldsa65Verify);
            }
        }

        const flow = this.settings.vlesses[0].flow;
        if (!ObjectUtil.isEmpty(flow)) {
            params.set("flow", flow);
        }
```

REALITY 的分享链接**不能**把 `address` 改成 SNI：SNI 是伪装目标的域名，客户端要连的仍然是你服务器的 IP。TLS 那一支改 `address` 是既有行为（用户填的是自己的真域名），保持不变。

- [ ] **Step 2: 清理 genVmessLink 与 genTrojanLink 的 xtls 残留**

```bash
grep -n 'xtls\|isXTls\|stream.http\|stream.quic' web/assets/js/model/xray.js
```

逐个删除。`genVmessLink` 里若有 `net: "http"` / `"quic"` 的分支一并删掉。

- [ ] **Step 3: 语法检查与控制台验证**

```bash
node --check web/assets/js/model/xray.js && echo "语法 OK"
```

浏览器控制台，用一个配好的 REALITY 入站：

```js
app.inbounds.find(i => i.toInbound && i.toInbound().stream.isReality)
```

找不到时直接在编辑弹窗里点「二维码 / 复制链接」，把生成的链接贴出来检查，应形如：

```
vless://<uuid>@<ip>:443?type=tcp&security=reality&sni=<伪装域名>&pbk=<公钥>&sid=<shortId>&fp=chrome&spx=%2F&flow=xtls-rprx-vision#<备注>
```

- [ ] **Step 4: 用后端的解析器交叉验证（关键）**

面板自己的 `util/link` 能解析 reality 链接，正好用来验证生成端与解析端对齐。新建 `util/link/inbound_link_roundtrip_test.go`：

```go
package link

import "testing"

// 面板前端生成的分享链接，必须能被面板自己的解析器读回来。
// 生成端在 web/assets/js/model/xray.js 的 genVLESSLink，解析端在
// outbound.go——两边的参数名靠这个测试对齐，改错一个名字就会红。
func TestGeneratedRealityLinkParsesBack(t *testing.T) {
	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:443" +
		"?type=tcp&security=reality&sni=www.example.com&pbk=THEPBK&sid=0123456789abcdef" +
		"&fp=chrome&spx=%2F&flow=xtls-rprx-vision#remark"

	res, err := ParseLink(raw)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	re := streamSub(t, res, "realitySettings")
	for k, want := range map[string]string{
		"publicKey": "THEPBK", "shortId": "0123456789abcdef",
		"serverName": "www.example.com", "fingerprint": "chrome", "spiderX": "/",
	} {
		if got := re[k]; got != want {
			t.Errorf("realitySettings[%q] = %v, want %q", k, got, want)
		}
	}
}
```

`streamSub` 是 `util/link/outbound_helpers_test.go` 里的既有 helper，直接复用。

```bash
go test ./util/link/ -run TestGeneratedRealityLinkParsesBack -v
```

期望：PASS。

- [ ] **Step 5: 提交**

```bash
make verify
git add web/assets/js/model/xray.js util/link/inbound_link_roundtrip_test.go
git commit -m "$(cat <<'MSG'
feat(xray.js): 分享链接支持 reality / vision / ECH 参数

参数名以 util/link/outbound.go 的解析端为准（pbk/sid/sni/fp/spx/pqv/ech），
新增的 roundtrip 测试把生成端与解析端锁在一起——两边用不同的名字时，
链接看起来一切正常，客户端却连不上。

REALITY 的链接刻意不把 address 换成 SNI：SNI 是伪装目标的域名，客户端要
连的仍然是服务器 IP。TLS 那一支换 address 是既有行为，保持不变。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

### Task 13: 热交换待验证项与收尾验证

**Files:**
- Test: `web/service/xray_hot_reload_e2e_test.go`（追加用例）
- Modify（条件性）: `xray/hot_diff.go`

**Interfaces:**
- Consumes: `web/service/xray_hot_reload_e2e_test.go` 里既有的 `requireXrayRoutingService` 等 helper

设计文档 §7.3 记录的待验证项：VLESS + tcp + **TLS** + Vision 的入站不命中 `inboundUsesReality`，会走热交换的「先删后加」两条 RPC。删加能否正确重建一个 Vision 入站，**目前没有证据**。这个任务用测试给出结论，而不是猜。

- [ ] **Step 1: 读既有的 e2e 测试，复用它的骨架**

```bash
sed -n '1,80p' web/service/xray_hot_reload_e2e_test.go
grep -n '^func' web/service/xray_hot_reload_e2e_test.go
```

重点看它怎么起真实 xray、怎么判定「进程有没有重启」、`requireXrayRoutingService` 的 skip 条件。**照它的方式写**，不要另起一套。

- [ ] **Step 2: 追加用例**

在 `web/service/xray_hot_reload_e2e_test.go` 末尾追加。复用该文件既有的 helper：`requireXrayBinary` / `requirePgrep` / `requireXrayAPIPortFree` / `setupDB` / `xrayChildPID` / `waitForPort`（`accesslog_e2e_test.go:152`）/ `freePort`（`accesslog_e2e_test.go:141`）。

```go
// selfSignedCertForTest 生成一份仅用于测试的自签证书。Vision 要求外层是
// TLS 1.3，所以这个入站必须真的配上证书才能起来。证书写进 t.TempDir()，
// 测试结束自动清理——不要写进仓库。
func selfSignedCertForTest(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatalf("生成测试私钥: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hot-reload-vision.test"},
		DNSNames:     []string{"hot-reload-vision.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(crand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发测试证书: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// 设计文档 §7.3 的待验证项：VLESS + tcp + TLS + Vision 的入站不命中
// inboundUsesReality，会走热交换的「先删后加」两条 gRPC。删加能否正确
// 重建一个 Vision 入站，此前没有证据——这个测试给出结论。
//
// 断言分两层，缺一不可：
//   1. PID 不变     → 确实走了热应用，没有退回整进程重启
//   2. 端口仍能完成 TLS 握手 → 入站真的被重建了，而不是删掉之后没加回来
//
// 只断言 PID 不变是不够的：热应用「成功」但入站没加回来，PID 同样不变，
// 而所有用户已经断线——这正是本项目最忌讳的静默失效。
func TestHotReloadRebuildsVisionInbound(t *testing.T) {
	requireXrayBinary(t)
	requirePgrep(t)
	requireXrayAPIPortFree(t)
	setupDB(t)

	t.Cleanup(func() {
		lock.Lock()
		defer lock.Unlock()
		if p != nil {
			_ = p.Stop()
			p = nil
		}
		result = ""
	})

	certPath, keyPath := selfSignedCertForTest(t)
	visionPort := freePort(t)

	stream := fmt.Sprintf(`{"network":"tcp","security":"tls",`+
		`"tlsSettings":{"serverName":"hot-reload-vision.test",`+
		`"minVersion":"1.3","maxVersion":"1.3","alpn":["h2","http/1.1"],`+
		`"certificates":[{"certificateFile":%q,"keyFile":%q}]},`+
		`"tcpSettings":{"header":{"type":"none"}}}`, certPath, keyPath)

	in := &model.Inbound{
		UserId: 1, Port: visionPort, Protocol: model.VLESS, Enable: true,
		Tag:    "inbound-" + strconv.Itoa(visionPort),
		Listen: "127.0.0.1",
		Settings: `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811",` +
			`"flow":"xtls-rprx-vision","email":"before@e2e"}],"decryption":"none"}`,
		StreamSettings: stream,
		Sniffing:       "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}

	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	waitForPort(t, visionPort)
	pidBefore := xrayChildPID(t)
	t.Logf("Vision 入站已起来，PID=%d，端口=%d", pidBefore, visionPort)

	// 改一个只落在 Settings 里的字段，触发这个入站的热交换（先删后加）。
	in.Settings = `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811",` +
		`"flow":"xtls-rprx-vision","email":"after@e2e"}],"decryption":"none"}`
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("update inbound: %v", err)
	}
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}

	pidAfter := xrayChildPID(t)
	tlsOK := tlsHandshakeOK(t, visionPort)

	switch {
	case pidAfter == pidBefore && tlsOK:
		t.Log("结论：Vision 入站可以安全热交换——PID 未变且端口仍能完成 TLS 握手")
	case pidAfter == pidBefore && !tlsOK:
		t.Fatal("热应用报告成功、进程也没重启，但入站已经不在了——" +
			"这是静默断线，必须把 Vision 纳入强制重启判定（见 Step 3 结论 B）")
	default:
		t.Logf("结论：Vision 入站退回了整进程重启（PID %d -> %d）。"+
			"功能正确但不是热应用，应把 Vision 纳入 inboundNeedsRestart 让这个行为显式化",
			pidBefore, pidAfter)
	}
}

// tlsHandshakeOK 只验证对端能完成 TLS 握手，不验证证书链——证书是自签的。
// 握手成功即证明该入站仍在监听且 TLS 配置完好。
func tlsHandshakeOK(t *testing.T, port int) bool {
	t.Helper()
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", fmt.Sprintf("127.0.0.1:%d", port),
		&tls.Config{InsecureSkipVerify: true, ServerName: "hot-reload-vision.test"})
	if err != nil {
		t.Logf("TLS 握手失败: %v", err)
		return false
	}
	defer conn.Close()
	return true
}
```

需要往该文件的 import 块补：`crypto/ecdsa`、`crypto/elliptic`、`crypto/tls`、`crypto/x509`、`crypto/x509/pkix`、`encoding/pem`、`math/big`、`path/filepath`，以及把 `crypto/rand` 导入为 `crand`（该文件已有 `net/http` 等，注意不要与既有导入重名）。

- [ ] **Step 3: 跑测试，记录结论**

```bash
go test ./web/service/ -run TestHotReload -v
```

测试打印的日志会落在三种结论之一，按结论处理：

**结论 A —— PID 未变且 TLS 握手成功。** Vision 入站可以安全热交换。不改代码，只在 `xray/hot_diff.go` 的 `inboundUsesReality` 上方补一条注释：

```go
// 只判 Reality 而不判 Vision，是有实测依据的：VLESS + tcp + TLS + Vision
// 的入站经「先删后加」两条 RPC 能被正确重建，PID 不变且端口仍能完成 TLS
// 握手（2026-09-03，TestHotReloadRebuildsVisionInbound）。将来若发现 Vision
// 入站在热应用后失联，先回去跑那个测试，不要凭猜把它加进强制重启。
```

**结论 C —— PID 变了。** 热应用没生效、退回了整进程重启。功能是对的（`tryHotApply` 的失败兜底按设计工作），但这个行为目前是隐式的。把它显式化，处理方式同结论 B。

**结论 B —— PID 未变但 TLS 握手失败。** 这是静默断线，最坏的一种。测试会直接 `t.Fatal`。必须把 `inboundUsesReality` 扩展成 `inboundNeedsRestart`：

```go
// inboundNeedsRestart 判断这个入站的改动能否走 gRPC 控制面热应用。
// Reality 的鉴权器无法靠删+加可靠重建（3x-ui 实测结论）；Vision 入站
// 经本项目实测同样不行（见 xray_hot_reload_e2e_test.go 的对应用例）。
func inboundNeedsRestart(ib *InboundConfig) bool {
	return inboundUsesReality(ib) || inboundUsesVision(ib)
}
```

并把 `hot_diff.go:104` 与 `:128` 的调用点改为 `inboundNeedsRestart`。`inboundUsesVision` 解析 `ib.Settings` 里的 `clients[].flow`（VLESS 存的键名是 `clients`），写法参照 `inboundUsesReality` 对 `StreamSettings` 的解析：同样要容忍 JSON 解不开的情况，解不开时**返回 true（保守地要求重启）**而不是 false——判错方向的代价不对称，多一次重启只是断线一两秒，漏判则是永久性的配置不一致。

**三种结论都是可接受的产出**，重点是有证据。**不要为了让测试变绿而放宽断言**，尤其不要把结论 B 的 `t.Fatal` 改成 `t.Log`。

- [ ] **Step 4: 全量回归**

```bash
make verify
```

期望：全绿。若有测试失败，先区分是本次改动引入的还是任务开始前就存在的——后者不擅自修复，但要在最终报告里说明。

- [ ] **Step 5: 端到端手工验收**

`XUI_DEBUG=true go run main.go`，逐条走一遍：

1. 新建 vless + tcp + reality + vision 入站（端口 443），保存成功
2. 复制分享链接，导入到一个支持 REALITY 的客户端（v2rayN / NekoBox 的较新版本），确认能连通
3. 新建 vmess + ws + tls 入站（用自有域名与证书），保存成功，确认既有用法没被破坏
4. 编辑现有的任意一个老入站，保存，确认没有字段丢失
5. 域名分流页仍然正常，sniffing 的警告图标行为不变

第 2 步是唯一能证明整条链路真正可用的检查。**没做这一步，不能声称改造完成**——前面所有测试证明的都只是「配置合法」，不是「能连上」。

- [ ] **Step 6: 清理与最终检查**

```bash
git status
git diff --stat HEAD~13..HEAD
ls /private/tmp/claude-501/-Users-caryallen-Desktop-AetherUI-AetherUI-main/f69813d8-e9e6-460b-bffa-cdb27820aef4/scratchpad
```

确认：仓库里没有临时脚本、调试输出、测试用的 sqlite 改动残留；Task 11 Step 8 造的那条 `security=xtls` 测试数据已删除。

- [ ] **Step 7: 提交**

```bash
git add web/service/xray_hot_reload_e2e_test.go xray/hot_diff.go
git commit -m "$(cat <<'MSG'
test(xray): 确认 Vision 入站的热交换行为

设计阶段留下的待验证项：Vision（非 Reality）入站不命中 inboundUsesReality，
会走热交换的先删后加，而删加能否正确重建它没有证据。这个提交用真实
xray 给出结论，而不是靠猜把它归进强制重启。

<按实际结论三选一保留一行，删掉另外两行>
结论 A：实测可以安全热交换（PID 不变、TLS 握手正常），只补注释不改代码。
结论 B：实测热应用后入站失联，已扩展为 inboundNeedsRestart 并纳入 Vision 判定。
结论 C：实测退回整进程重启，已扩展为 inboundNeedsRestart 让该行为显式化。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01B1ZrUTcB8eTY9vS5fQYLu7
MSG
)"
```

---

## 附录：执行者常见误判

- **「CLAUDE.md 说新增字段要同步扩展 `Config.Equals`，我改一下。」** 不要改。`Settings`/`StreamSettings`/`Sniffing` 是 `json_util.RawMessage`，走 `bytes.Equal`，本次新增的字段全在这三块里，天然被覆盖。那条警告针对的是结构体里的具名字段。
- **「测试 SKIP 了，应该算通过。」** 不算。本机 `bin/xray-darwin-arm64` 在位，测试应当实跑。看到 SKIP 说明二进制缺失或路径不对，先修环境。
- **「`xray run -test` 返回 `Configuration OK`，说明配置没问题。」** 不一定。核心的三条 GFW 风险判定、Vision 对 TLS 1.3 的要求、uTLS 指纹的合法性，全都是 `LogWarning` 或运行期检查，`run -test` 一律放行。这也正是本计划要在表单层做联动禁用的原因。
- **「改完 JS 直接刷新页面没生效。」** 强缓存。`config/version` 没变时静态资源是 `max-age=31536000`。用硬刷新（Cmd+Shift+R），或确认 `XUI_DEBUG=true` 已生效（调试模式从磁盘读，但缓存头仍在）。
- **「弹窗里的按钮点了没反应。」** 大概率是 Vue 指令写在了 `#app` 之外。Vue 2 只编译 `el` 指向的那棵子树，落在外面的绑定完全静默。跑 `TestVueDirectivesLiveInsideAVueRoot`。
