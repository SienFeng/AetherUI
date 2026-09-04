# 入站协议抗封锁改造 设计

日期：2026-09-03
状态：待评审
定位：`2026-09-03-modernization-roadmap.md` §3「计划 04 — Reality 与现代入站能力」的详细设计。路线图是需求与全局约束的来源，本文档只负责把计划 04 展开到可实施的粒度。

---

## 0. 一句话

面板今天生成的入站配置里，有四个选项在当前核心上会**导致整份配置加载失败、全机断网**，同时缺失 REALITY、Vision、ECH 等全部现代抗封能力。本次把前者清掉、后者补上。

---

## 1. 问题

### 1.1 四个死选项

前端仍然提供，但当前核心在配置构建阶段直接报错：

| 前端位置 | 选项 | 核心的反应 |
|---|---|---|
| `web/html/xui/form/stream/stream_settings.html:9` | `network: http` | `infra/conf/transport_internet.go:33` → `PrintRemovedFeatureError("HTTP transport")` |
| 同上 `:10` | `network: quic` | 同文件 `:35` → `PrintRemovedFeatureError("QUIC transport")` |
| `web/assets/js/model/xray.js` 的 `isXTls` 及各协议表单的 xtls 开关 | `security: xtls` | 同文件 `:113` → `PrintRemovedFeatureError("Legacy XTLS")` |
| `web/assets/js/model/xray.js:43-46` 的 `FLOW_CONTROL` | `xtls-rprx-origin` / `xtls-rprx-direct` | `infra/conf/vless.go:51` 的白名单只有 `vless.XRV` 和 `""` |

按 CLAUDE.md 记载的「xray 加载配置是全有或全无」，任一入站命中其中之一，整份 `bin/config.json` 加载失败，该机所有用户一起断网。

`ValidateInboundReplacing`（`web/service/routing_validate.go`）目前能在落库前拦下，但它是 **fail open** 的——二进制缺失、老版本不认 `run -test`、执行超时一律放行。它是辅助手段，不能作为「这四个选项无害」的理由。何况它们本身是纯死代码：留在界面上只有害处。

### 1.2 抗封能力缺口

- **无 REALITY**。有意思的是后端已经为它铺好了路：`xray/hot_diff.go:164` 的 `inboundUsesReality()` 与 `:104`/`:128` 的强制重启分支已经在位，`util/link/outbound.go:592` 也能解析 reality 分享链接。只有前端从来生成不出 `realitySettings`。
- **无 `xtls-rprx-vision`**。这是当前核心唯一接受的 flow。
- **TLS 参数只有 `serverName` + `certificates`**。缺 `alpn` / `minVersion` / `maxVersion` / `cipherSuites` / `rejectUnknownSni` / `certificates[].ocspStapling`。
- **无 ECH**。核心支持 `echServerKeys` / `echConfigList`（`infra/conf/transport_security.go:316-317`），能把 SNI 本身加密。
- **sniffing 只有一个开关**。`web/html/xui/form/sniffing.html` 全文 15 行，`destOverride` 前端完全没暴露——而域名分流子系统硬依赖它（见 `2026-09-02-domain-routing-design.md`）。

---

## 2. 已查证的技术前提

以下全部对照 `github.com/xtls/xray-core v1.260327.1-0.20260728075948-5ca6f4b7d4dc`（即 26.7.28，与 `bin/xray-linux-*` 同一 commit）的模块缓存源码，行号为该版本内的实际行号。**不是推断，也不凭记忆。**

### 2.1 传输方式白名单

`infra/conf/transport_internet.go:16-41`：接受 `raw`/`tcp`、`xhttp`/`splithttp`、`kcp`/`mkcp`、`grpc`、`ws`/`websocket`、`httpupgrade`、`hysteria`。`h2`/`h3`/`http` 与 `quic` 返回 `PrintRemovedFeatureError`。

`ws`（`:27`）与 `grpc`（`:24`）已被标记 `PrintNonRemovalDeprecatedFeatureWarning`，核心推荐 XHTTP——但 XHTTP 不在本次范围（§3.2）。

`StreamConfig` 结构体（`:43-62`）已无 `httpSettings` / `quicSettings` / `xtlsSettings` 三个键。

### 2.2 安全层白名单与 REALITY 的传输限制

`infra/conf/transport_internet.go:85-118`：只接受 `""`/`none`、`tls`、`reality`；`xtls` 返回 `PrintRemovedFeatureError`。

`:100`：**REALITY 只支持 `tcp`(RAW)、`splithttp`(XHTTP)、`grpc`**。本次不做 XHTTP，因此 REALITY 在面板里只对 `network ∈ {tcp, grpc}` 可选。

### 2.3 REALITY 服务端的必填项与格式约束

`infra/conf/transport_security.go:54-181`（`REALITYConfig.Build`）：

- `target` 与 `dest` 是**别名**（`:59-61`，`Target` 非 nil 时覆盖 `Dest`）。
- `serverNames` 非空（`:95`），`privateKey` 非空且必须是 32 字节 `base64.RawURLEncoding`（`:98-101`）。
- `shortIds` 非空（`:136`），单个 ≤16 字符（`:140`）且必须是 **hex**（`:145` 走 `hex.Decode`）。
- `xver` 只接受 0/1/2（`:92`）。
- `mldsa65Seed` 若填写，必须是 32 字节 `base64.RawURLEncoding`，且**不得与 `privateKey` 相同**（`:155-160`）。

### 2.4 核心内置的三条 GFW 风险判定

这三条是本设计中伪装目标预置列表、`minClientVer` 默认值与 REALITY 端口策略的**直接依据**。三条都是 `LogWarning` 而非 `error`——**配置照常加载、`xray run -test` 照常返回 `Configuration OK`**，所以面板不主动提示的话，管理员永远不会知道自己踩了哪条：

**（a）`transport_security.go:118`** — `minClientVer` 留空时默认 `[]byte{26, 3, 27}`，即只接受 Xray v26.3.27 及以上的客户端。紧邻的 `:116`（位于 `if c.MinClientVer != ""` 分支内，因此**填任何非空值都会触发，不分调高调低**）写着：

> `REALITY: Changing "minClientVer" will increase the likelihood of your server's IP being blocked by the GFW`

也就是说**「兼容旧客户端」与「不被墙」在 REALITY 上直接冲突**。已与需求方确认：**安全优先，表单默认留空，交给核心用 26.3.27**。旧客户端必须升级。

**（b）`transport_security.go:163-169`** — 核心遍历 **`serverNames`**（不是伪装目标 `target`／`dest`）做后缀与子串匹配，命中即警告「提高被 GFW 封锁的概率」：

```
后缀 .ru / .ir / .cn，或包含 apple / icloud / microsoft
```

**这里有一个极易读错的地方，本文档初稿就读错了**：核心那条日志写的是 `Choosing "<sn>" as the target will increase...`，措辞指向「target」，但它遍历的切片是 `config.ServerNames`。判定的是 **serverNames**，与 `target`／`dest` 无关。

```go
for _, sn := range config.ServerNames {      // ← 不是 config.Dest
    sn = strings.ToLower(sn)
    if strings.HasSuffix(sn, ".ru") || strings.HasSuffix(sn, ".ir") || strings.HasSuffix(sn, ".cn") ||
        strings.Contains(sn, "apple") || strings.Contains(sn, "icloud") || strings.Contains(sn, "microsoft") {
        errors.LogWarning(context.Background(), `REALITY: Choosing "`, sn, `" as the target will increase ...`)
    }
}
```

按 target 判定会在「target 安全、serverNames 危险」时**漏报**——恰好复现本子系统要消灭的那类静默失效。此错误在实现期由代码审查发现并纠正（2026-09-04），面板侧的判定实现在 `web/html/xui/inbound_modal.html` 的 `realityTargetRisky`，读的是 `serverNames`（逗号分隔逐项判断，任一命中即警告）。

**推论：预置的伪装目标列表与 serverNames 都必须避开这六类。** 社区里广为流传的 `www.microsoft.com`、`swdist.apple.com`、`gateway.icloud.com` 全部踩雷，不得进入预置列表。

**（c）`infra/conf/xray.go:177-179`** — REALITY 入站的端口列表必须**恰好只有一个端口且等于 443**，否则警告：

> `REALITY: Listening on non-443 ports will increase the likelihood of your server's IP being blocked by the GFW`

这条是 2026-09-03 用本地 `bin/xray-darwin-arm64`（26.7.28，commit `5ca6f4b`）实跑 `run -test` 时从日志里发现的，源码位置随后核实。**推论：选中 REALITY 时表单必须把端口默认为 443**，且端口非 443 时就地给出与核心一致的警告。AetherUI 现在新建入站默认取 `RandomUtil.randomIntRange(10000, 60000)`（`web/assets/js/model/xray.js` 的 `Inbound` 构造函数），对 REALITY 而言这个默认值恰好是最坏选择。

同一次实测还确认了一件事：**`realitySettings` 下的 `settings` 子对象（存客户端半边参数）不会被核心拒绝**，返回 `Configuration OK`。§4.3 采用 3x-ui 这一约定的前提因此成立，不是假设。

### 2.5 Vision flow 的约束

`infra/conf/vless.go:50-53`（入站级）与 `:326-330`（用户级）：flow 白名单只有 `vless.XRV`（`xtls-rprx-vision`）与 `""`。

`proxy/vless/inbound/inbound.go:573`：Vision 在运行期要求外层 TLS 版本为 **TLS 1.3**，否则报 `failed to use xtls-rprx-vision, found outer tls version` 并断开。

**这是一个「配置能加载、但用户连不上」的静默故障**——`xray run -test` 过得去，`ValidateInboundReplacing` 拦不住。必须在表单层强制：选 Vision 时若 `security=tls`，`minVersion` 锁死为 `1.3`。

`inbound.go:557`：Vision 不支持 UDP 请求（`doesn't support UDP`）。这是运行期行为，面板不需要处理，但要在界面提示里说明。

### 2.6 TLS 与 ECH 字段

`TLSConfig`（`transport_security.go:300-319`）的完整字段已确认，本次采用其中：`serverName`、`alpn`、`minVersion`、`maxVersion`、`cipherSuites`、`rejectUnknownSni`、`certificates`、`echServerKeys`。

`TLSCertConfig`（`:248-257`）：本次新增 `ocspStapling`。

ECH：`:391-398`，`echServerKeys` 走 `base64.StdEncoding` 解码（**注意与 REALITY 的 `RawURLEncoding` 不同**），`echConfigList` 原样传给客户端侧。

### 2.7 三种密钥生成都可在 Go 内原生实现

| 密钥 | 核心实现 | AetherUI 复刻所需 | 依赖状态 |
|---|---|---|---|
| REALITY x25519 | `main/commands/all/curve25519.go:38-58` | `crypto/ecdh` + `encoding/base64` | 标准库 |
| REALITY ML-DSA-65 | `main/commands/all/mldsa65.go:30-46` | `circl/sign/mldsa/mldsa65` | `go.mod` indirect 已有 `cloudflare/circl v1.6.4` |
| TLS ECH | `main/commands/all/tls/ech.go:42-149` | **标准库 `crypto/hpke`**（Go 1.24+）+ `golang.org/x/crypto/cryptobyte` | 前者是标准库，后者已在 indirect 区 |

**结论：不新增任何 Go 模块**，只把已有的两个 indirect 依赖（`cloudflare/circl`、`golang.org/x/crypto`）提升为 direct。这满足路线图 §4.1「新增任何其他依赖前，先说明标准库与现有依赖为何不足」。

**本文档初稿在 ECH 一行写的是 `circl/hpke`，是错的**：xray-core 的 `main/commands/all/tls/ech.go:5` 导入的是标准库 `crypto/hpke`（Go 1.24 起提供，本仓库 `go.mod` 声明 go 1.27.0），其 `HKDFSHA256()` / `AES128GCM()` / `DHKEM()` 等函数式 API 与 circl 的类型化常量 API 并不兼容。此错误在实现期由 TDD 暴露并纠正（2026-09-03），实际实现用的是标准库。

**不采用 exec `bin/xray x25519` 的方案**，理由：`bin/xray-darwin-arm64` 在 `.gitignore` 中，本地开发环境没有该文件，密钥生成会直接失败；而密钥生成与配置校验不同，**不能 fail open**——生成不出来就是生成不出来。

x25519 复刻时注意 `curve25519.go:50-52` 的私钥 clamping（`&= 248` / `&= 127` / `|= 64`），漏掉会生成出核心不接受的私钥。

### 2.8 AetherUI 侧已确认无需改动的三处

- **`xray/inbound.go:18` 的 `InboundConfig.Equals`**：`Settings` / `StreamSettings` / `Sniffing` 都是 `json_util.RawMessage` 且走 `bytes.Equal`。本次新增的字段全部落在这三块里，**天然被覆盖**。CLAUDE.md 里那条「新增字段漏改 `Equals` 会导致永久静默失效」的坑本次绕开了，但这个结论必须写进实施计划，避免执行者出于谨慎去改一个不该改的地方。
- **`xray/hot_diff.go:164` 的 `inboundUsesReality`**：已用 `strings.EqualFold` 识别 `reality`，REALITY 入站自动走整进程重启。
- **`web/service/routing_validate.go` 的 `ValidateInboundReplacing`**：走完整生成配置送 `xray run -test`，新配置自动纳入校验，无需扩展。

---

## 3. 范围

### 3.1 做

1. REALITY 安全层（入站），含 x25519 与 ML-DSA-65 密钥生成
2. VLESS 的 `xtls-rprx-vision` flow
3. TLS 参数补全：`alpn` / `minVersion` / `maxVersion` / `cipherSuites` / `rejectUnknownSni` / `ocspStapling` / 客户端指纹
4. **ECH**：`echServerKeys`（服务端）+ `echConfigList`（写进分享链接）+ 密钥生成
5. sniffing 的 `destOverride` 复选框
6. 清除 §1.1 的四个死选项，并处理存量数据（§6）
7. 分享链接生成同步

### 3.2 明确不做

写在这里是为了防止执行期范围蔓延：

- **XHTTP 传输**。需求方明确排除。代价是 ws/grpc 继续带着核心的 deprecated 警告运行——功能正常，只是不是核心推荐的路径。
- **VLESS Encryption**（`mlkem768` 后量子加密，核心有 `vlessenc` 子命令）。客户端支持面比 `minClientVer 26.3.27` 还窄。
- **fallback 配置**（路线图第三梯队 f）。见 §10。
- **REALITY 伪装目标在线探测器**（3x-ui 的 `internal/web/service/reality_scan.go`）。见 §10。
- **`limitFallbackUpload` / `limitFallbackDownload`**（`transport_security.go:171-181`）。REALITY 回落流的限速，与抗封无关，且 AetherUI 已有独立的入站限速机制（`downMbit`/`upMbit`），两套限速并存会造成语义混淆。

---

## 4. 前端模型设计（`web/assets/js/model/xray.js`）

采用**就地扩展现有类**，不拆文件、不引入 security 抽象层。理由是路线图 §4.4 与 CLAUDE.md 的「遵循现有架构，不另建平行体系」：拆文件要改 `inbounds.html` 的 script 引用并承担 `cur_ver` 强缓存风险，收益（文件行数）不抵风险。

预计 `xray.js` 从 1537 行增至约 1900 行。

### 4.1 常量

```
FLOW_CONTROL          只留 VISION: "xtls-rprx-vision"，删除 ORIGIN / DIRECT
TLS_VERSION_OPTION    1.0 / 1.1 / 1.2 / 1.3
TLS_CIPHER_OPTION     auto（空串）+ §4.7 的 TLS 1.2 套件名
UTLS_FINGERPRINT      chrome / firefox / safari / ios / android / edge / 360 / qq / random / randomized
ALPN_OPTION           h3 / h2 / http/1.1
SNIFFING_OPTION       http / tls / quic / fakedns
REALITY_TARGET_PRESETS  见 §4.4
```

`UTLS_FINGERPRINT` 不含 `unsafe` 与 `hellogolang`：核心在 `transport_security.go:181` 显式拒绝这两个值。注意该校验位于 `REALITYConfig.Build` 的 **else 分支**，即只在 REALITY 作为**出站**时生效——入站侧的 `fingerprint` 核心根本不读，它纯粹是面板存给分享链接用的。因此这个约束 `xray run -test` 验不出来，必须由前端的选项列表保证，否则用户拿到的链接会被自己的客户端拒绝。

### 4.2 `TlsStreamSettings` 扩展

新增 `alpn`（数组）、`minVersion`、`maxVersion`、`cipherSuites`、`rejectUnknownSni`、`echServerKeys`，以及一个 `settings` 子对象存**客户端半边**（`fingerprint`、`allowInsecure`、`echConfigList`）——仅用于生成分享链接，不影响服务端行为。这是 3x-ui 的既有做法（`frontend/src/schemas/protocols/security/tls.ts`）。

`TlsStreamSettings.Cert` 新增 `ocspStapling`。

默认值：`alpn=['h2','http/1.1']`、`minVersion='1.2'`、`maxVersion='1.3'`、`cipherSuites=''`（auto）、`rejectUnknownSni=false`、`ocspStapling=3600`。

### 4.3 新增 `RealityStreamSettings`

服务端字段：`show`、`xver`、`target`、`serverNames`、`privateKey`、`mldsa65Seed`、`minClientVer`、`maxClientVer`、`maxTimeDiff`、`shortIds`。
客户端半边（`settings` 子对象，供分享链接用）：`publicKey`、`fingerprint`、`serverName`、`spiderX`、`mldsa65Verify`。

两条实现约束：

- **`serverNames` 与 `shortIds` 在 JS 里存逗号分隔字符串**（表单友好），`toJson()` 时 split → trim → 去空 → **去重且保持首次出现顺序**。保持顺序而非排序，是因为路线图 §4.2 的「生成逐字节确定」——只要规则确定即可，但绝不能依赖遍历 map 的顺序。
- **`fromJson()` 必须做 `dest` → `target` 的别名映射**。这条抄自 3x-ui 的注释（`frontend/src/schemas/protocols/security/reality.ts:29-38`）记录的真实教训：老配置或外部工具写的是 `dest`，不映射的话面板读进来 `target` 为空，用户编辑后一保存就把工作正常的 `dest` 抹掉，**直到下一次重启才暴露**。核心侧两者确实是别名（§2.3）。

### 4.4 伪装目标预置列表

**选择标准**（可审计、可复现，不是「我觉得好」）：

1. 不命中核心的高风险判定（§2.4b）：非 `.ru`/`.ir`/`.cn` 后缀，不含 `apple`/`icloud`/`microsoft`
2. 满足 3x-ui `reality_scan.go:298` 的四项可用性判据：`TLS13 && H2 && X25519 && CertValid`
3. 在目标机房（日本东京）有低延迟节点

**实施期必须逐个实测这四项后才能写进代码**，不得凭记忆写死候选域名——这是路线图 §4.1 「不凭记忆写死」的同一条纪律。文档只定标准，候选值在实施计划里给出并附实测记录。

表单同时提供自定义输入，并在输入命中 §2.4b 六类时前端就地给出黄色警告（与核心的日志警告一致）。

### 4.5 `StreamSettings`

- 删除 `isXTls` getter/setter 与 `toJson()` 里的 `xtlsSettings` 分支
- 删除 `httpSettings` / `quicSettings` 分支及 `HttpStreamSettings` / `QuicStreamSettings` 两个类
- 新增 `reality` 成员与 `isReality` getter/setter
- `security` 变为三态 `none` / `tls` / `reality`

### 4.6 `Sniffing`

新增 `destOverride` 的复选框绑定，选项 `http` / `tls` / `quic` / `fakedns`。默认保持现有的 `['http','tls']`（`web/assets/js/model/xray.js` 的 `Sniffing` 构造函数），不改变新建入站的既有默认行为。

### 4.7 `cipherSuites`：一个必须降级处理的字段

核对 `transport/internet/tls/config.go:451-464` 后得出两条结论，都与直觉相反：

1. **无法识别的套件名被静默丢弃**。解析逻辑是 `if v, ok := id[n]; ok { append }`，**没有 else 分支、不报错、不记日志**。名字拼错一个就是少一个套件；全拼错则 `config.CipherSuites` 为空，等同于回落 Go 默认——用户以为限制了套件，实际什么也没限制，而 `xray run -test` 照样通过。
2. **对 TLS 1.3 完全无效**。Go 的 `crypto/tls` 不接受 TLS 1.3 的套件配置（`Config.CipherSuites` 只作用于 TLS 1.0–1.2）。而 Vision 强制 TLS 1.3（§2.5）、REALITY 也走 1.3——**在本次主推的两条路径上，`cipherSuites` 是零作用的装饰**。魔改版把它摆在显眼位置属于 cargo cult。

因此设计上做三点降级处理：

- **必须是下拉多选，不能是自由文本框**（否则第 1 条的静默丢弃无从防范），提交时用 `:` 连接
- **候选值只放实际生效的 TLS 1.2 套件**，即 `crypto/tls.CipherSuites()` 中 `SupportedVersions` 含 `771` 的那些：`TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256`、`TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384`、`TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256`、`TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384`、`TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256`、`TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256`。**不提供 `InsecureCipherSuites()` 里的任何一个**（RC4、3DES、CBC 系列），它们只会削弱伪装
- 界面注明「仅影响 TLS 1.2 握手；Vision 与 REALITY 走 TLS 1.3，此项不生效」，避免用户误以为调了它就更安全

实施时套件名以 `crypto/tls.CipherSuites()` 的实际输出为准，不照抄本文档——Go 版本变更可能增删条目。

---

## 5. 表单设计

需求方已确认形态：**一键安全默认 + 高级折叠**。

### 5.1 文件改动

| 文件 | 改动 |
|---|---|
| `stream/stream_settings.html` | 删 http/quic 选项，network 只留 tcp/kcp/ws/grpc |
| `stream/stream_http.html`、`stream/stream_quic.html` | 删除 |
| `tls_settings.html` | 重写：基础区（域名、证书）+ 高级折叠区 |
| `reality_settings.html` | **新增** |
| `sniffing.html` | 加 `destOverride` 复选框组 |
| `protocol/vless.html` | flow 下拉改为 `""` / `xtls-rprx-vision`；触发条件由 `v-if="inbound.xtls"` 改为按 §5.3 判定 |
| `protocol/vmess.html` 等 | 删除各协议表单里的 xtls 开关 |
| `inbound.html` | 顶部加存量数据警告条（§6）；安全层改为三态选择器 |

### 5.2 一键默认的具体行为

选中 REALITY 时自动：调后端生成 x25519 密钥对填入 `privateKey`/`publicKey`、生成一个随机 hex `shortId`、`spiderX` 置 `/`、`fingerprint` 置 `chrome`、`flow` 置 `xtls-rprx-vision`、**端口置 443**（§2.4c）、`minClientVer` **留空**（§2.4a）。

端口非 443 时在端口输入框旁就地警告，文案与核心日志一致。这一条尤其重要：核心只在启动日志里 `LogWarning`，而面板从不展示 xray 的启动日志（CLAUDE.md 记载面板状态本身就可能是假的），管理员实际上没有任何途径看到这条警告。

高级折叠区放 `show` / `xver` / `minClientVer` / `maxClientVer` / `maxTimeDiff` / `mldsa65Seed`，留空即安全默认。`minClientVer` 输入框旁给红字风险提示，文案直接引用核心的警告原文。

### 5.3 按核心约束联动禁用

不是摆着让用户随便选，而是按 §2 查证的约束灰掉不合法组合：

- REALITY 仅在 `network ∈ {tcp, grpc}` 时可选（§2.2）
- Vision 仅在 `protocol === vless` 且 `security ∈ {tls, reality}` 时可选（§2.5）
- 选中 Vision 且 `security === tls` 时，`minVersion` 锁死 `1.3` 且不可改（§2.5）
- ECH 仅在 `security === tls` 时可用（REALITY 自带 SNI 伪装，`realitySettings` 无 ECH 字段）

**这一节是本设计里唯一能挡住「配置能加载但用户连不上」类故障的地方**，`xray run -test` 对它们无能为力。

---

## 6. 存量数据处理

死选项直接从下拉里删掉有一个必须正面处理的副作用：编辑一个 `network=http` 的老入站时，`a-select` 会显示空白，用户随手一保存就把它**静默改成别的传输方式**——用户可见行为的静默变更，正是本项目最忌讳的失效模式。

设计：

1. **下拉里移除，但 `fromJson` 保留识别能力**。`security === 'xtls'`、`network ∈ {http, quic}`、`flow ∈ {xtls-rprx-origin, xtls-rprx-direct}` 都要能被读进模型而不丢失。
2. `Inbound` 增加只读 getter `deprecatedFeatures`，返回当前配置命中的已移除特性列表。
3. `inbound.html` 顶部据此渲染红色 `a-alert`，逐条说明「此项在当前 Xray 核心已移除，必须改为 X」。
4. **警告存在时禁用保存按钮**，强制用户显式改掉，而不是静默接受一个被改写的配置。

注意 Vue 指令必须写在 `el` 指向的根元素之内（路线图 §4.2，`web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 守着这条）。

---

## 7. 后端设计

### 7.1 新增密钥生成接口

挂在已有的需登录的 `/server/*` 路由下（`web/controller/server.go`）：

| 路由 | 返回 |
|---|---|
| `POST {basePath}server/getNewX25519Cert` | `{privateKey, publicKey}` |
| `POST {basePath}server/getNewMldsa65` | `{seed, verify}` |
| `POST {basePath}server/getNewEchCert` | `{echServerKeys, echConfigList}`，接受可选的 `serverName`（默认 `cloudflare-ech.com`，对齐 `ech.go:38`） |

路由命名对齐 3x-ui，便于将来对照。实现放 `web/service/server.go`（当前 301 行，容得下这三个函数，不新建文件）。

编码方式必须与核心一致，三者不同，**不能混**：

- x25519 私钥/公钥：`base64.RawURLEncoding`（`curve25519.go:17`）
- ML-DSA-65 seed/verify：`base64.RawURLEncoding`（`mldsa65.go:42-44`）
- ECH configList/serverKeys：`base64.StdEncoding`（`ech.go:95-96`）

### 7.2 无需改动的部分

见 §2.8。实施计划必须显式写明「不改 `Equals`」及其理由，防止执行者出于对 CLAUDE.md 那条警告的谨慎而做出不必要的改动。

### 7.3 一个待验证项，不写成结论

VLESS + tcp + **TLS** + Vision（非 REALITY）的入站不会命中 `inboundUsesReality`，因此会走热交换的「先删后加」两条 RPC。**删加能否正确重建一个 Vision 入站，目前没有证据。**

处理方式：用 e2e 测试确认（§9），而不是猜。若确认不可行，就把「入站使用 Vision flow」也纳入强制重启的判定，与 `inboundUsesReality` 并列。

---

## 8. 分享链接

`web/assets/js/model/xray.js` 的 `genVLESSLink`：

- 新增 `security=reality` 分支，输出 `pbk` / `sid` / `sni` / `fp` / `spx`，有 `mldsa65Verify` 时输出 `pqv`
- `security=tls` 分支补 `alpn` / `fp`，有 ECH 时输出 `ech`
- 输出 `flow`

参数名以 `util/link/outbound.go` 的解析端为准（同一项目的两端必须对齐）：`:681-682` 的 `pbk`/`sid`，`:461` 与 `:675` 的 `ech` → `echConfigList`，`outbound_helpers_test.go:113` 覆盖的 `pbk`/`sid`/`sni`/`fp`/`spx`/`pqv`。

`genVmessLink` / `genTrojanLink` 同步删除 xtls 相关输出。

---

## 9. 测试

遵循路线图 §4.5，尤其是「**一个测试必须在没有其修复时失败**」。

| 测试 | 覆盖 |
|---|---|
| `TestAllTemplatesParse`（已有） | 新增/改写的模板能被解析。`getHtmlTemplate` 吞掉 `ParseFS` 错误，`go build` 发现不了模板语法错误 |
| `TestVueDirectivesLiveInsideAVueRoot`（已有） | 新模板的 Vue 指令在根元素内 |
| 新增：密钥生成 | 生成的 x25519 / ML-DSA-65 / ECH 密钥能被真实核心接受（组进完整配置跑 `run -test`）。缺二进制时 **skip 并说明原因**，不伪造通过 |
| 新增：Reality + Vision 完整入站 | 通过 `ValidateInboundReplacing` |
| 新增：热交换正确性 | §7.3 的待验证项。参照 `web/service/xray_hot_reload_e2e_test.go` 的既有做法，核心不提供所需能力时 skip 并说明，而不是以「PID 变了」这种和真实缺陷无法区分的形式失败 |
| 新增：存量数据识别 | `security=xtls` / `network=http` 的老配置能被 `fromJson` 读进来并被 `deprecatedFeatures` 标记出来 |
| 新增：`dest`→`target` 别名 | §4.3 的映射，round-trip 后 `dest` 不丢失 |

`web/service` 的 `TestMain` 会 `os.Chdir` 到仓库根，新增测试不得依赖包内相对路径（路线图 §4.5）。

---

## 10. 与路线图计划 04 的差异

路线图 §3 的计划 04 覆盖「第三梯队 a（Reality / `xtls-rprx-vision`）、f（fallback 配置）」。本设计相对它有三处调整，均需需求方知悉：

| 项 | 路线图 | 本设计 | 理由 |
|---|---|---|---|
| **ECH** | 未提及 | **纳入** | 需求方明确追加。核心已支持，能加密 SNI，是 TLS 路线上的一层实质伪装 |
| **fallback 配置**（第三梯队 f） | 计划 04 覆盖 | **推迟** | 需求方本轮批准的范围不含它。fallback 是 VLESS 的独立子特性（`infra/conf/vless.go:24-31` 的 `VLessInboundFallback`），有自己的表单、校验与端口协调逻辑；且 REALITY 已通过 `target` 提供等效的回落伪装。建议单独成一轮 |
| **伪装目标在线探测器**（3x-ui `reality_scan.go`） | 计划 04 的参考实现 | **推迟**，只做静态预置列表 | 探测器需要面板主动向外发起 TLS 连接，是新的出网行为与新的攻击面。本轮先用 §4.4 的可审计标准 + 实测确认的静态列表 |

需求方若要把 fallback 或探测器拉回本轮，在评审时说明即可。

---

## 11. 风险

1. **旧客户端连不上 REALITY**（§2.4a）。这是核心的默认行为，且已确认取「安全优先」。上线前需要通知现有用户升级客户端。本设计不改变面板的 TLS 入站，因此存量的 VMess+WS+TLS 用户不受影响。
2. **§7.3 的热交换未知项**。若结论是「不可行」，Vision 入站的每次改动都会触发整进程重启（约 1~2 秒断线），而不是热应用。这是可接受的降级，但要写进实施计划的验收口径。
3. **`xray.js` 体量增长**。1537 → 约 1900 行。本轮不拆分（§4 的理由），但应在路线图里记一笔，作为将来前端重构的输入。
4. **ECH 的实际收益依赖客户端支持**。服务端配好不等于链路生效：客户端必须拿到 `echConfigList` 且自身支持 ECH。本设计通过分享链接的 `ech` 参数下发，但对不支持 ECH 的客户端是静默无效——**不是故障，但也不是保护**。界面需要说明这一点，避免用户误以为开了就一定生效。
