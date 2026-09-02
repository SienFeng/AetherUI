# 域名分流管理 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让管理员在 Web 面板上配置「哪个用户访问哪批域名时走哪个落地节点或直接黑洞」，无需手写 xray 配置。

**Architecture:** 新增三张表（域名组 / 出站节点 / 分流规则）。`xrayTemplateConfig` 手工模板保持不动，只在 `XrayService.GetXrayConfig()` 里把出站与路由规则增量注入到模板末尾。落地节点通过粘贴分享链接录入，解析器移植自 3x-ui 的 `internal/util/link` 包。

**Tech Stack:** Go 1.21+ / Gin 1.7.1 / GORM 1.21 + SQLite / Vue 2.6 + ant-design-vue 1.7.2（服务端 Go 模板渲染，无前端构建步骤）

**Spec:** `docs/superpowers/specs/2026-09-02-domain-routing-design.md`

## Global Constraints

- **模块名是 `a-ui`**，所有 import 路径形如 `a-ui/util/link`、`a-ui/web/service`。
- **`go.mod` 的 go 指令必须提升到 `1.21`**（Task 1 完成）。移植包用了 `any`（Go 1.18）与 `maps.Copy`（Go 1.21）。CI 用 Go 1.22，本地 1.27，均满足。
- **生成的出站 tag 一律加 `a-ui-` 前缀**，禁止与手工模板的 tag 冲突。黑洞出站 tag 固定为 `a-ui-block`。
- **配置生成必须确定性**：相同的库内容两次生成必须得到逐字节相同的 JSON。规则一律按 `Priority, Id` 升序排列。违反此约束会导致 `Config.Equals` 恒为 false，xray 被 10 秒 cron 反复重启。
- **绝不输出条件残缺的规则**：域名列表为空、出站不存在或已禁用、入站不存在时，整条规则跳过。原因见 spec §2——xray 对 `domain: []` 不报错，会把规则退化成「劫持该入站全部流量」。
- **本仓库当前不是 git 仓库**。若要执行计划中的 commit 步骤，需先征得用户同意并 `git init`；否则把每个 commit 步骤当作「跑一次 `go build ./... && go vet ./...` 的检查点」。**不得擅自 `git init`。**
- 项目原本零测试。本计划新增的测试是正式回归测试，需保留。
- 现有代码的中文风格保持一致：controller 层用 `jsonMsg(c, "添加", err)` 这类中文动词拼接消息。

---

### Task 1: 移植 link 解析包并提升 go 指令

**Files:**
- Create: `util/link/outbound.go`（复制自 `/Users/caryallen/Desktop/3x-ui-main/internal/util/link/outbound.go`）
- Create: `util/link/outbound_test.go`、`util/link/outbound_helpers_test.go`、`util/link/outbound_fuzz_test.go`（同上目录复制）
- Modify: `go.mod:3`

**Interfaces:**
- Consumes: 无
- Produces: `link.ParseLink(link string) (*link.ParseResult, error)`；`link.Outbound`（即 `map[string]any`）；`link.ParseResult{Outbound Outbound; Identity string}`；`link.SuggestTag(prefix, remark string, idx int) string`；`link.SlugRemark(remark string) string`；包内私有辅助 `defaultPort`、`decodeHash`、`padBase64`、`base64DecodeFlexible`、`buildStream` 供 Task 2 使用。

- [ ] **Step 1: 复制源文件**

```bash
cd /Users/caryallen/Desktop/AetherUI/AetherUI-main
mkdir -p util/link
cp /Users/caryallen/Desktop/3x-ui-main/internal/util/link/outbound.go util/link/
cp /Users/caryallen/Desktop/3x-ui-main/internal/util/link/outbound_test.go util/link/
cp /Users/caryallen/Desktop/3x-ui-main/internal/util/link/outbound_helpers_test.go util/link/
cp /Users/caryallen/Desktop/3x-ui-main/internal/util/link/outbound_fuzz_test.go util/link/
```

包名已经是 `package link`，无需改动。该包只依赖标准库，不引用 3x-ui 的任何内部包，因此不需要改 import。

- [ ] **Step 2: 在文件头补上来源与许可声明**

在 `util/link/outbound.go` 第 1 行之前插入：

```go
// This file is derived from 3x-ui (github.com/mhsanaei/3x-ui), licensed under
// GPL-3.0, file internal/util/link/outbound.go. AetherUI is also GPL-3.0.
// Modifications: socks support lives in socks.go alongside this file.

```

- [ ] **Step 3: 提升 go 指令**

把 `go.mod` 第 3 行 `go 1.16` 改成：

```
go 1.21
```

其余内容一行不动。

- [ ] **Step 4: 跑移植过来的测试，确认全绿**

Run: `go test ./util/link/`
Expected: `ok  	a-ui/util/link`

- [ ] **Step 5: 确认没有引入任何第三方依赖**

Run: `go mod tidy && git diff --stat go.sum 2>/dev/null || wc -l go.sum`
Expected: `go.sum` 行数不变（43086 字节 / 原有行数）。若 `go mod tidy` 想新增依赖，说明复制错了文件，回退重来。

- [ ] **Step 6: 确认整个项目仍能编译**

Run: `go build ./... && go vet ./...`
Expected: 无输出（成功）

- [ ] **Step 7: Commit**

```bash
git add go.mod util/link/
git commit -m "feat: 移植 3x-ui 的分享链接解析包，go 指令提升至 1.21"
```

---

### Task 2: 新增 socks 链接解析

**Files:**
- Create: `util/link/socks.go`
- Create: `util/link/socks_test.go`
- Modify: `util/link/outbound.go`（`ParseLink` 的 switch，约在第 113-129 行）

**Interfaces:**
- Consumes: Task 1 的 `defaultPort`、`decodeHash`、`base64DecodeFlexible`、`buildStream`、`Outbound`、`ParseResult`
- Produces: `parseSocks(link string) (*ParseResult, error)`（包内私有）；`ParseLink` 新增识别 `socks://`、`socks5://`、`socks4://`、`socks4a://` 四种 scheme

- [ ] **Step 1: 写失败的测试**

创建 `util/link/socks_test.go`：

```go
package link

import (
	"encoding/base64"
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
	if _, err := ParseLink("socks5://"); err == nil {
		t.Error("expected error for empty body")
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./util/link/ -run TestParseSocks -v`
Expected: 全部 FAIL，错误信息为 `unsupported link scheme`

- [ ] **Step 3: 实现 parseSocks**

创建 `util/link/socks.go`：

```go
package link

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// parseSocks parses socks share links. The format is not standardized, so we
// try three shapes in order:
//
//	1. socks://<base64(user:pass)>@host:port#remark   (v2rayN)
//	2. socks://<base64(user:pass@host:port)>#remark   (legacy)
//	3. socks5://user:pass@host:port#remark            (plain, credentials optional)
func parseSocks(rawLink string) (*ParseResult, error) {
	rest := rawLink
	for _, scheme := range []string{"socks5://", "socks4a://", "socks4://", "socks://"} {
		if strings.HasPrefix(rest, scheme) {
			rest = strings.TrimPrefix(rest, scheme)
			break
		}
	}

	remark := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		remark = decodeHash(rest[i+1:])
		rest = rest[:i]
	}
	if rest == "" {
		return nil, fmt.Errorf("socks: empty body")
	}

	// Shape 2: whole body is base64 and decodes to something with credentials.
	if !strings.Contains(rest, "@") {
		if dec, err := base64DecodeFlexible(rest); err == nil && strings.Contains(dec, "@") {
			rest = dec
		}
	}

	cred, hostport := "", rest
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		cred, hostport = rest[:at], rest[at+1:]
	}

	// Shape 1: the credential part itself may be base64. A base64 blob never
	// contains ":", so its absence is the signal to try decoding.
	if cred != "" && !strings.Contains(cred, ":") {
		if dec, err := base64DecodeFlexible(cred); err == nil && strings.Contains(dec, ":") {
			cred = dec
		}
	}

	user, pass := "", ""
	if cred != "" {
		if i := strings.Index(cred, ":"); i >= 0 {
			user, pass = cred[:i], cred[i+1:]
		} else {
			user = cred
		}
	}

	// net.SplitHostPort handles the [::1]:1080 bracket form correctly.
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		host, portStr = strings.Trim(hostport, "[]"), ""
	}
	if host == "" {
		return nil, fmt.Errorf("socks: missing host")
	}
	port := defaultPort(portStr, 1080)

	server := map[string]any{"address": host, "port": port}
	if user != "" || pass != "" {
		server["users"] = []any{map[string]any{"user": user, "pass": pass}}
	}

	ob := Outbound{
		"protocol":       "socks",
		"tag":            remark,
		"settings":       map[string]any{"servers": []any{server}},
		"streamSettings": buildStream("tcp", "none"),
	}
	identity := "socks:" + user + "@" + host + ":" + strconv.Itoa(port)
	return &ParseResult{Outbound: ob, Identity: identity}, nil
}
```

- [ ] **Step 4: 在 ParseLink 中注册**

在 `util/link/outbound.go` 的 `ParseLink` switch 里，把

```go
	case strings.HasPrefix(link, "wireguard://"), strings.HasPrefix(link, "wg://"):
		return parseWireguard(link)
```

改成

```go
	case strings.HasPrefix(link, "wireguard://"), strings.HasPrefix(link, "wg://"):
		return parseWireguard(link)
	case strings.HasPrefix(link, "socks://"), strings.HasPrefix(link, "socks5://"),
		strings.HasPrefix(link, "socks4://"), strings.HasPrefix(link, "socks4a://"):
		return parseSocks(link)
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./util/link/ -v`
Expected: 新增的 6 个 socks 测试与移植包原有测试全部 PASS

- [ ] **Step 6: 用真实 xray 校验产物合法**

把一条解析结果手工拼进配置试跑，确认 xray 接受：

```bash
cat > /tmp/socks-check.json <<'EOF'
{
  "inbounds": [{"tag":"in","port":10001,"protocol":"vless",
    "settings":{"clients":[{"id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}],"decryption":"none"}}],
  "outbounds": [{"protocol":"freedom","settings":{}},
    {"tag":"a-ui-node-1","protocol":"socks",
     "settings":{"servers":[{"address":"1.2.3.4","port":1080,
       "users":[{"user":"alice","pass":"secret"}]}]}}]
}
EOF
./bin/xray-darwin-arm64 run -test -c /tmp/socks-check.json
```

Expected: `Configuration OK.`（结束后 `rm /tmp/socks-check.json`）

- [ ] **Step 7: Commit**

```bash
git add util/link/socks.go util/link/socks_test.go util/link/outbound.go
git commit -m "feat: 支持 socks 分享链接解析"
```

---

### Task 3: 数据模型与迁移

**Files:**
- Create: `database/model/routing.go`
- Create: `database/routing_migrate_test.go`
- Modify: `database/db.go:38-46`（`initSetting` 之后新增函数）、`database/db.go:66-77`（`InitDB` 内的调用序列）

**Interfaces:**
- Consumes: 无
- Produces: `model.DomainGroup`、`model.OutboundNode`、`model.RoutingRule` 三个结构体；常量 `model.ActionProxy = "proxy"`、`model.ActionBlock = "block"`、`model.BlockOutboundTag = "a-ui-block"`、`model.OutboundTagPrefix = "a-ui"`

- [ ] **Step 1: 写失败的测试**

创建 `database/routing_migrate_test.go`：

```go
package database

import (
	"path/filepath"
	"testing"

	"a-ui/database/model"
)

func TestInitDBCreatesRoutingTables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := GetDB()
	for _, tbl := range []any{&model.DomainGroup{}, &model.OutboundNode{}, &model.RoutingRule{}} {
		if !db.Migrator().HasTable(tbl) {
			t.Errorf("table for %T was not created", tbl)
		}
	}
}

func TestOutboundNodeTagIsUnique(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := GetDB()
	first := &model.OutboundNode{Tag: "a-ui-hk", Remark: "HK", Protocol: "socks", Config: "{}", Enable: true}
	if err := db.Create(first).Error; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	dup := &model.OutboundNode{Tag: "a-ui-hk", Remark: "HK2", Protocol: "socks", Config: "{}", Enable: true}
	if err := db.Create(dup).Error; err == nil {
		t.Error("duplicate tag was accepted, want unique constraint violation")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./database/ -v`
Expected: 编译失败，`undefined: model.DomainGroup`

- [ ] **Step 3: 写模型**

创建 `database/model/routing.go`：

```go
package model

// 分流规则的动作。
const (
	ActionProxy = "proxy"
	ActionBlock = "block"
)

// 生成的出站 tag 一律带此前缀，与用户手写模板的 tag 隔离。
const (
	OutboundTagPrefix = "a-ui"
	BlockOutboundTag  = "a-ui-block"
)

// DomainGroup 是一批可复用的域名集合。
type DomainGroup struct {
	Id     int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Remark string `json:"remark" form:"remark"`
	// Domains 是 JSON 字符串数组，元素为 xray 原生域名语法：
	// domain:openai.com / full:chat.openai.com / geosite:openai / regexp:.*\.oaistatic\.com
	Domains string `json:"domains" form:"domains"`
}

// OutboundNode 是一个落地代理服务器。
type OutboundNode struct {
	Id       int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Tag      string `json:"tag" form:"tag" gorm:"unique"`
	Remark   string `json:"remark" form:"remark"`
	Protocol string `json:"protocol" form:"protocol"`
	// Config 是完整的 xray outbound JSON，tag 字段以本表的 Tag 为准。
	Config string `json:"config" form:"config"`
	Enable bool   `json:"enable" form:"enable"`
}

// RoutingRule 把「哪个入站访问哪个域名组」连到「哪个出站或黑洞」。
type RoutingRule struct {
	Id     int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Remark string `json:"remark" form:"remark"`
	// InboundId 为 0 表示对所有入站生效。
	InboundId     int    `json:"inboundId" form:"inboundId"`
	DomainGroupId int    `json:"domainGroupId" form:"domainGroupId"`
	Action        string `json:"action" form:"action"`
	// OutboundId 仅在 Action 为 ActionProxy 时有意义。
	OutboundId int  `json:"outboundId" form:"outboundId"`
	Priority   int  `json:"priority" form:"priority"`
	Enable     bool `json:"enable" form:"enable"`
}
```

- [ ] **Step 4: 挂上迁移**

在 `database/db.go` 的 `initSetting` 函数之后新增：

```go
func initRouting() error {
	if err := db.AutoMigrate(&model.DomainGroup{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.OutboundNode{}); err != nil {
		return err
	}
	return db.AutoMigrate(&model.RoutingRule{})
}
```

在 `InitDB` 中，把

```go
	err = initSetting()
	if err != nil {
		return err
	}

	return nil
```

改成

```go
	err = initSetting()
	if err != nil {
		return err
	}
	err = initRouting()
	if err != nil {
		return err
	}

	return nil
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./database/ -v`
Expected: 两个测试均 PASS

- [ ] **Step 6: Commit**

```bash
git add database/model/routing.go database/db.go database/routing_migrate_test.go
git commit -m "feat: 新增域名组、出站节点、分流规则三张表"
```

---

### Task 4: 域名组 CRUD

**Files:**
- Create: `web/service/routing_domain.go`
- Create: `web/service/routing_domain_test.go`

**Interfaces:**
- Consumes: Task 3 的 `model.DomainGroup`
- Produces: `service.DomainGroupService` 及其方法：`GetAll() ([]*model.DomainGroup, error)`、`Get(id int) (*model.DomainGroup, error)`、`Add(g *model.DomainGroup) error`、`Update(g *model.DomainGroup) error`、`Del(id int) error`；包级函数 `service.ParseDomains(raw string) ([]string, error)`、`service.EncodeDomains(list []string) (string, error)`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/routing_domain_test.go`：

```go
package service

import (
	"path/filepath"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

func setupDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func TestParseDomainsAcceptsNativeSyntax(t *testing.T) {
	raw := "domain:openai.com\nfull:chat.openai.com\ngeosite:openai\nregexp:.*\\.oaistatic\\.com"
	got, err := ParseDomains(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4: %v", len(got), got)
	}
	if got[0] != "domain:openai.com" {
		t.Errorf("got[0] = %q", got[0])
	}
}

func TestParseDomainsSkipsBlankLinesAndTrims(t *testing.T) {
	got, err := ParseDomains("  domain:a.com  \n\n\n  geosite:openai\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "domain:a.com" || got[1] != "geosite:openai" {
		t.Errorf("got = %v, want [domain:a.com geosite:openai]", got)
	}
}

func TestParseDomainsRejectsUnknownPrefix(t *testing.T) {
	if _, err := ParseDomains("wat:openai.com"); err == nil {
		t.Error("expected error for unknown prefix")
	}
}

func TestParseDomainsRejectsEmptyResult(t *testing.T) {
	if _, err := ParseDomains("   \n  \n"); err == nil {
		t.Error("expected error for empty domain list")
	}
}

func TestDomainGroupCRUD(t *testing.T) {
	setupDB(t)
	s := DomainGroupService{}

	encoded, err := EncodeDomains([]string{"domain:openai.com", "geosite:openai"})
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	g := &model.DomainGroup{Remark: "ChatGPT", Domains: encoded}
	if err := s.Add(g); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if g.Id == 0 {
		t.Fatal("Add did not assign an Id")
	}

	all, err := s.GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 || all[0].Remark != "ChatGPT" {
		t.Fatalf("GetAll = %v", all)
	}

	g.Remark = "OpenAI"
	if err := s.Update(g); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(g.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Remark != "OpenAI" {
		t.Errorf("Remark = %q, want OpenAI", got.Remark)
	}

	if err := s.Del(g.Id); err != nil {
		t.Fatalf("Del: %v", err)
	}
	all, _ = s.GetAll()
	if len(all) != 0 {
		t.Errorf("after Del, GetAll = %v, want empty", all)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run 'TestParseDomains|TestDomainGroup' -v`
Expected: 编译失败，`undefined: ParseDomains`

- [ ] **Step 3: 实现**

创建 `web/service/routing_domain.go`：

```go
package service

import (
	"encoding/json"
	"strings"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
)

// xray 支持的域名匹配前缀。不带前缀的裸域名 xray 也接受（等价于子串匹配），
// 但容易误伤，这里要求显式前缀。
var domainPrefixes = []string{"domain:", "full:", "geosite:", "regexp:", "ext:"}

// ParseDomains 把用户在 textarea 中一行一条录入的域名解析成列表。
func ParseDomains(raw string) ([]string, error) {
	lines := strings.Split(raw, "\n")
	list := make([]string, 0, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		ok := false
		for _, p := range domainPrefixes {
			if strings.HasPrefix(item, p) && len(item) > len(p) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, common.NewError("域名格式不支持，必须以 domain: / full: / geosite: / regexp: / ext: 开头:", item)
		}
		list = append(list, item)
	}
	if len(list) == 0 {
		return nil, common.NewError("域名列表不能为空")
	}
	return list, nil
}

// EncodeDomains 把域名列表序列化为入库格式。
func EncodeDomains(list []string) (string, error) {
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeDomains 是 EncodeDomains 的逆操作。库中数据损坏时返回错误而非空列表，
// 避免生成条件残缺的路由规则。
func DecodeDomains(encoded string) ([]string, error) {
	var list []string
	if err := json.Unmarshal([]byte(encoded), &list); err != nil {
		return nil, err
	}
	return list, nil
}

type DomainGroupService struct {
}

func (s *DomainGroupService) GetAll() ([]*model.DomainGroup, error) {
	db := database.GetDB()
	groups := make([]*model.DomainGroup, 0)
	err := db.Model(model.DomainGroup{}).Order("id asc").Find(&groups).Error
	if err != nil {
		return nil, err
	}
	return groups, nil
}

func (s *DomainGroupService) Get(id int) (*model.DomainGroup, error) {
	db := database.GetDB()
	group := &model.DomainGroup{}
	err := db.Model(model.DomainGroup{}).First(group, id).Error
	if err != nil {
		return nil, err
	}
	return group, nil
}

func (s *DomainGroupService) Add(group *model.DomainGroup) error {
	db := database.GetDB()
	return db.Save(group).Error
}

func (s *DomainGroupService) Update(group *model.DomainGroup) error {
	old, err := s.Get(group.Id)
	if err != nil {
		return err
	}
	old.Remark = group.Remark
	old.Domains = group.Domains
	db := database.GetDB()
	return db.Save(old).Error
}

func (s *DomainGroupService) Del(id int) error {
	db := database.GetDB()
	return db.Delete(model.DomainGroup{}, id).Error
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./web/service/ -run 'TestParseDomains|TestDomainGroup' -v`
Expected: 5 个测试全部 PASS

- [ ] **Step 5: Commit**

```bash
git add web/service/routing_domain.go web/service/routing_domain_test.go
git commit -m "feat: 域名组服务与域名语法校验"
```

---

### Task 5: 出站节点 CRUD 与链接解析

**Files:**
- Create: `web/service/routing_outbound.go`
- Create: `web/service/routing_outbound_test.go`

**Interfaces:**
- Consumes: Task 1/2 的 `link.ParseLink`、`link.SuggestTag`；Task 3 的 `model.OutboundNode`、`model.OutboundTagPrefix`
- Produces: `service.OutboundNodeService` 及其方法：`GetAll()`、`GetEnabled()`、`Get(id int)`、`AddFromLink(rawLink, remark string) (*model.OutboundNode, error)`、`AddFromJSON(cfg, remark string) (*model.OutboundNode, error)`、`Update(n *model.OutboundNode) error`、`Del(id int) error`、`allocTag(remark string) (string, error)`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/routing_outbound_test.go`：

```go
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run TestAddFrom -v`
Expected: 编译失败，`undefined: OutboundNodeService`

- [ ] **Step 3: 实现**

创建 `web/service/routing_outbound.go`：

```go
package service

import (
	"encoding/json"
	"fmt"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
	"a-ui/util/link"
)

type OutboundNodeService struct {
}

func (s *OutboundNodeService) GetAll() ([]*model.OutboundNode, error) {
	db := database.GetDB()
	nodes := make([]*model.OutboundNode, 0)
	err := db.Model(model.OutboundNode{}).Order("id asc").Find(&nodes).Error
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// GetEnabled 按 Id 升序返回启用的节点。顺序固定是配置生成确定性的前提。
func (s *OutboundNodeService) GetEnabled() ([]*model.OutboundNode, error) {
	db := database.GetDB()
	nodes := make([]*model.OutboundNode, 0)
	err := db.Model(model.OutboundNode{}).Where("enable = ?", true).Order("id asc").Find(&nodes).Error
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

func (s *OutboundNodeService) Get(id int) (*model.OutboundNode, error) {
	db := database.GetDB()
	node := &model.OutboundNode{}
	err := db.Model(model.OutboundNode{}).First(node, id).Error
	if err != nil {
		return nil, err
	}
	return node, nil
}

// allocTag 生成一个未被占用的、带 a-ui- 前缀的 tag。
//
// 不能用自增 Id 拼 tag：Tag 有唯一约束，必须在 INSERT 之前就确定，
// 而那一刻 Id 尚未分配。
//
// 注意 link.SuggestTag 只在 remark 为空时才拿 idx 兜底；remark 非空时
// 它对任何 idx 都返回同一个值，所以重名必须靠这里自己追加序号。
// 又，link.SlugRemark 的正则是 [^\p{L}\p{N}]+，会保留中文，生成的 tag
// 形如 a-ui-香港-b-节点 —— 已实测 xray 接受非 ASCII tag，无需转写。
func (s *OutboundNodeService) allocTag(remark string) (string, error) {
	db := database.GetDB()
	base := link.SuggestTag(model.OutboundTagPrefix, remark, 1)
	for idx := 1; idx < 1000; idx++ {
		candidate := base
		if idx > 1 {
			candidate = fmt.Sprintf("%s-%d", base, idx)
		}
		var count int64
		if err := db.Model(model.OutboundNode{}).Where("tag = ?", candidate).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return candidate, nil
		}
	}
	return "", common.NewError("无法为该备注分配唯一 tag，请更换备注")
}

// persist 把解析好的 outbound 写库，并把 tag 强制改写成本表分配的值。
func (s *OutboundNodeService) persist(ob map[string]any, protocol, remark string) (*model.OutboundNode, error) {
	tag, err := s.allocTag(remark)
	if err != nil {
		return nil, err
	}
	ob["tag"] = tag
	encoded, err := json.Marshal(ob)
	if err != nil {
		return nil, err
	}
	node := &model.OutboundNode{
		Tag:      tag,
		Remark:   remark,
		Protocol: protocol,
		Config:   string(encoded),
		Enable:   true,
	}
	db := database.GetDB()
	if err := db.Save(node).Error; err != nil {
		return nil, err
	}
	return node, nil
}

// AddFromLink 解析分享链接并落库。
func (s *OutboundNodeService) AddFromLink(rawLink string, remark string) (*model.OutboundNode, error) {
	result, err := link.ParseLink(rawLink)
	if err != nil {
		return nil, common.NewError("解析分享链接失败:", err)
	}
	protocol, _ := result.Outbound["protocol"].(string)
	if protocol == "" {
		return nil, common.NewError("解析结果缺少 protocol 字段")
	}
	return s.persist(map[string]any(result.Outbound), protocol, remark)
}

// AddFromJSON 直接接收一段 xray outbound JSON（高级模式）。
func (s *OutboundNodeService) AddFromJSON(cfg string, remark string) (*model.OutboundNode, error) {
	var ob map[string]any
	if err := json.Unmarshal([]byte(cfg), &ob); err != nil {
		return nil, common.NewError("outbound JSON 格式错误:", err)
	}
	protocol, _ := ob["protocol"].(string)
	if protocol == "" {
		return nil, common.NewError("outbound JSON 缺少 protocol 字段")
	}
	return s.persist(ob, protocol, remark)
}

// Update 只允许改备注、启用状态和配置内容，Tag 一经分配不可变——
// 改 tag 会让所有引用它的规则悬空，而 xray 对此不报错。
func (s *OutboundNodeService) Update(node *model.OutboundNode) error {
	old, err := s.Get(node.Id)
	if err != nil {
		return err
	}
	if node.Config != "" && node.Config != old.Config {
		var ob map[string]any
		if err := json.Unmarshal([]byte(node.Config), &ob); err != nil {
			return common.NewError("outbound JSON 格式错误:", err)
		}
		ob["tag"] = old.Tag
		encoded, err := json.Marshal(ob)
		if err != nil {
			return err
		}
		old.Config = string(encoded)
		if p, ok := ob["protocol"].(string); ok && p != "" {
			old.Protocol = p
		}
	}
	old.Remark = node.Remark
	old.Enable = node.Enable
	db := database.GetDB()
	return db.Save(old).Error
}

func (s *OutboundNodeService) Del(id int) error {
	db := database.GetDB()
	return db.Delete(model.OutboundNode{}, id).Error
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./web/service/ -run 'TestAddFrom|TestGetEnabled' -v`
Expected: 6 个测试全部 PASS

- [ ] **Step 5: Commit**

```bash
git add web/service/routing_outbound.go web/service/routing_outbound_test.go
git commit -m "feat: 出站节点服务，支持分享链接与 JSON 两种录入方式"
```

---

### Task 6: 分流规则 CRUD 与引用完整性

**Files:**
- Create: `web/service/routing_rule.go`
- Create: `web/service/routing_rule_test.go`

**Interfaces:**
- Consumes: Task 3 的 `model.RoutingRule`、`model.ActionProxy`、`model.ActionBlock`；Task 4 的 `DomainGroupService`；Task 5 的 `OutboundNodeService`
- Produces: `service.RoutingRuleService` 及其方法：`GetAll()`、`GetEnabled()`、`Get(id int)`、`Add(r *model.RoutingRule) error`、`Update(r *model.RoutingRule) error`、`Del(id int) error`、`CheckDomainGroupRefs(groupId int) error`、`CheckOutboundRefs(outboundId int) error`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/routing_rule_test.go`：

```go
package service

import (
	"testing"

	"a-ui/database/model"
)

func newTestGroup(t *testing.T, remark string) *model.DomainGroup {
	t.Helper()
	encoded, err := EncodeDomains([]string{"geosite:openai"})
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	g := &model.DomainGroup{Remark: remark, Domains: encoded}
	if err := (&DomainGroupService{}).Add(g); err != nil {
		t.Fatalf("Add group: %v", err)
	}
	return g
}

func TestAddRuleRejectsMissingDomainGroup(t *testing.T) {
	setupDB(t)
	s := RoutingRuleService{}
	err := s.Add(&model.RoutingRule{DomainGroupId: 999, Action: model.ActionBlock, Enable: true})
	if err == nil {
		t.Error("expected error when domain group does not exist")
	}
}

func TestAddRuleRejectsProxyWithoutOutbound(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: 0, Enable: true})
	if err == nil {
		t.Error("expected error when proxy rule has no outbound")
	}
}

func TestAddRuleRejectsUnknownAction(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, Action: "drop", Enable: true})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

func TestAddBlockRuleWithGlobalInbound(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	s := RoutingRuleService{}
	r := &model.RoutingRule{Remark: "全局封禁", InboundId: 0, DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true}
	if err := s.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if r.Id == 0 {
		t.Fatal("Add did not assign an Id")
	}
}

func TestCheckDomainGroupRefsBlocksDeletion(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckDomainGroupRefs(g.Id); err == nil {
		t.Error("expected error: domain group is referenced by a rule")
	}
	if err := s.CheckDomainGroupRefs(g.Id + 1); err != nil {
		t.Errorf("unreferenced group should be deletable, got %v", err)
	}
}

func TestCheckOutboundRefsBlocksDeletion(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{
		DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckOutboundRefs(node.Id); err == nil {
		t.Error("expected error: outbound is referenced by a rule")
	}
}

func TestGetEnabledRulesSortedByPriorityThenId(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	// 故意乱序插入
	for _, p := range []int{20, 10, 10} {
		if err := s.Add(&model.RoutingRule{
			DomainGroupId: g.Id, Action: model.ActionBlock, Priority: p, Enable: true,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	rules, err := s.GetEnabled()
	if err != nil {
		t.Fatalf("GetEnabled: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("len = %d, want 3", len(rules))
	}
	if rules[0].Priority != 10 || rules[1].Priority != 10 || rules[2].Priority != 20 {
		t.Fatalf("priorities = %d,%d,%d; want 10,10,20",
			rules[0].Priority, rules[1].Priority, rules[2].Priority)
	}
	if rules[0].Id > rules[1].Id {
		t.Error("rules with equal priority must be ordered by Id ascending")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run 'TestAddRule|TestCheck|TestGetEnabledRules' -v`
Expected: 编译失败，`undefined: RoutingRuleService`

- [ ] **Step 3: 实现**

创建 `web/service/routing_rule.go`：

```go
package service

import (
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
)

type RoutingRuleService struct {
	domainGroupService DomainGroupService
	outboundService    OutboundNodeService
}

func (s *RoutingRuleService) GetAll() ([]*model.RoutingRule, error) {
	db := database.GetDB()
	rules := make([]*model.RoutingRule, 0)
	err := db.Model(model.RoutingRule{}).Order("priority asc, id asc").Find(&rules).Error
	if err != nil {
		return nil, err
	}
	return rules, nil
}

// GetEnabled 按 priority、id 升序返回启用的规则。
// 这个顺序是配置生成确定性的前提，不可改成不稳定的排序。
func (s *RoutingRuleService) GetEnabled() ([]*model.RoutingRule, error) {
	db := database.GetDB()
	rules := make([]*model.RoutingRule, 0)
	err := db.Model(model.RoutingRule{}).Where("enable = ?", true).
		Order("priority asc, id asc").Find(&rules).Error
	if err != nil {
		return nil, err
	}
	return rules, nil
}

func (s *RoutingRuleService) Get(id int) (*model.RoutingRule, error) {
	db := database.GetDB()
	rule := &model.RoutingRule{}
	err := db.Model(model.RoutingRule{}).First(rule, id).Error
	if err != nil {
		return nil, err
	}
	return rule, nil
}

// validate 在写库前挡住会生成残缺规则的输入。
func (s *RoutingRuleService) validate(rule *model.RoutingRule) error {
	if _, err := s.domainGroupService.Get(rule.DomainGroupId); err != nil {
		return common.NewError("域名组不存在:", rule.DomainGroupId)
	}
	switch rule.Action {
	case model.ActionBlock:
		return nil
	case model.ActionProxy:
		if rule.OutboundId <= 0 {
			return common.NewError("走节点的规则必须指定出站节点")
		}
		if _, err := s.outboundService.Get(rule.OutboundId); err != nil {
			return common.NewError("出站节点不存在:", rule.OutboundId)
		}
		return nil
	default:
		return common.NewError("未知的动作:", rule.Action)
	}
}

func (s *RoutingRuleService) Add(rule *model.RoutingRule) error {
	if err := s.validate(rule); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Save(rule).Error
}

func (s *RoutingRuleService) Update(rule *model.RoutingRule) error {
	if err := s.validate(rule); err != nil {
		return err
	}
	old, err := s.Get(rule.Id)
	if err != nil {
		return err
	}
	old.Remark = rule.Remark
	old.InboundId = rule.InboundId
	old.DomainGroupId = rule.DomainGroupId
	old.Action = rule.Action
	old.OutboundId = rule.OutboundId
	old.Priority = rule.Priority
	old.Enable = rule.Enable
	db := database.GetDB()
	return db.Save(old).Error
}

func (s *RoutingRuleService) Del(id int) error {
	db := database.GetDB()
	return db.Delete(model.RoutingRule{}, id).Error
}

// CheckDomainGroupRefs 在删除域名组前调用。域名组一旦消失，引用它的规则
// domain 会变成空列表——而 xray 把空条件当作「无限制」，规则会从
// 「访问这批域名走某节点」退化成「该入站全部流量走某节点」，且不报错。
func (s *RoutingRuleService) CheckDomainGroupRefs(groupId int) error {
	db := database.GetDB()
	var count int64
	err := db.Model(model.RoutingRule{}).Where("domain_group_id = ?", groupId).Count(&count).Error
	if err != nil {
		return err
	}
	if count > 0 {
		return common.NewError("该域名组仍被", count, "条分流规则引用，请先删除这些规则")
	}
	return nil
}

// CheckOutboundRefs 在删除出站节点前调用。出站消失后规则会静默回落到
// 默认出站（直连），封禁与分流都会失效且无任何报错。
func (s *RoutingRuleService) CheckOutboundRefs(outboundId int) error {
	db := database.GetDB()
	var count int64
	err := db.Model(model.RoutingRule{}).
		Where("outbound_id = ? and action = ?", outboundId, model.ActionProxy).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count > 0 {
		return common.NewError("该出站节点仍被", count, "条分流规则引用，请先删除这些规则")
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./web/service/ -run 'TestAddRule|TestAddBlock|TestCheck|TestGetEnabledRules' -v`
Expected: 7 个测试全部 PASS

- [ ] **Step 5: 把引用检查接到删除入口**

修改 `web/service/routing_domain.go` 的 `Del`，改成：

```go
func (s *DomainGroupService) Del(id int) error {
	ruleService := RoutingRuleService{}
	if err := ruleService.CheckDomainGroupRefs(id); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Delete(model.DomainGroup{}, id).Error
}
```

修改 `web/service/routing_outbound.go` 的 `Del`，改成：

```go
func (s *OutboundNodeService) Del(id int) error {
	ruleService := RoutingRuleService{}
	if err := ruleService.CheckOutboundRefs(id); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Delete(model.OutboundNode{}, id).Error
}
```

- [ ] **Step 6: 跑全部 service 测试**

Run: `go test ./web/service/ -v`
Expected: 全部 PASS（Task 4/5/6 的测试都在内）

- [ ] **Step 7: Commit**

```bash
git add web/service/routing_rule.go web/service/routing_rule_test.go web/service/routing_domain.go web/service/routing_outbound.go
git commit -m "feat: 分流规则服务与引用完整性检查"
```

---

### Task 7: 配置合成注入

这是整个功能的核心。生成错了不会有任何报错，只会让流量默默走错地方，所以测试要求最严。

**Files:**
- Create: `web/service/routing_inject.go`
- Create: `web/service/routing_inject_test.go`
- Modify: `web/service/xray.go:54-76`（`GetXrayConfig` 末尾）

**Interfaces:**
- Consumes: Task 4/5/6 的三个 service；`xray.Config`、`json_util.RawMessage`、`InboundService.GetAllInbounds`
- Produces: `service.RoutingInjector` 及方法 `Inject(cfg *xray.Config) error`；内部方法 `buildOutbounds(existing []any) ([]any, error)`、`buildRules() (blockRules []any, proxyRules []any, err error)`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/routing_inject_test.go`：

```go
package service

import (
	"encoding/json"
	"strconv"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

const testTemplate = `{
  "outbounds": [
    {"protocol":"freedom","settings":{}},
    {"protocol":"blackhole","settings":{},"tag":"blocked"}
  ],
  "routing": {"rules": [
    {"type":"field","inboundTag":["api"],"outboundTag":"api"}
  ]}
}`

func newTemplateConfig(t *testing.T) *xray.Config {
	t.Helper()
	cfg := &xray.Config{}
	if err := json.Unmarshal([]byte(testTemplate), cfg); err != nil {
		t.Fatalf("unmarshal template: %v", err)
	}
	return cfg
}

func decodeOutbounds(t *testing.T, cfg *xray.Config) []map[string]any {
	t.Helper()
	var raw []map[string]any
	if err := json.Unmarshal(cfg.OutboundConfigs, &raw); err != nil {
		t.Fatalf("decode outbounds: %v", err)
	}
	return raw
}

func decodeRules(t *testing.T, cfg *xray.Config) []map[string]any {
	t.Helper()
	var routing struct {
		Rules []map[string]any `json:"rules"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
		t.Fatalf("decode routing: %v", err)
	}
	return routing.Rules
}

// newTestInbound 建一个启用的入站，tag 按现网规则由端口算出。
func newTestInbound(t *testing.T, port int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Enable: true,
		Tag:      "inbound-" + strconv.Itoa(port),
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	return in
}

func TestInjectAppendsBlockOutboundAndKeepsFreedomFirst(t *testing.T) {
	setupDB(t)
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	if obs[0]["protocol"] != "freedom" {
		t.Errorf("first outbound = %v, want freedom (it is xray's default outbound)", obs[0]["protocol"])
	}
	last := obs[len(obs)-1]
	if last["tag"] != model.BlockOutboundTag {
		t.Errorf("last outbound tag = %v, want %s", last["tag"], model.BlockOutboundTag)
	}
}

func TestInjectSkipsRuleWithEmptyDomainGroup(t *testing.T) {
	setupDB(t)
	// 直接建一条引用不存在域名组的规则，绕过 service 校验，模拟脏数据
	rule := &model.RoutingRule{DomainGroupId: 999, Action: model.ActionBlock, Enable: true}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 1 {
		t.Fatalf("rule count = %d, want 1 (only the template rule); a rule with no domains "+
			"would hijack all traffic for its inbound", len(rules))
	}
}

func TestInjectSkipsProxyRuleWhenOutboundDisabled(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}
	node.Enable = false
	if err := (&OutboundNodeService{}).Update(node); err != nil {
		t.Fatalf("Update node: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := len(decodeRules(t, cfg)); got != 1 {
		t.Errorf("rule count = %d, want 1; rules pointing at a disabled outbound must be skipped", got)
	}
}

func TestInjectBlockRulesComeBeforeProxyRules(t *testing.T) {
	setupDB(t)
	banned := newTestGroup(t, "违规域名")
	chatgpt := newTestGroup(t, "ChatGPT")
	node, _ := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	rs := RoutingRuleService{}
	// 先插 proxy 规则，priority 更小，以证明排序不是靠插入顺序或 priority
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: chatgpt.Id, Action: model.ActionProxy, OutboundId: node.Id,
		Priority: 1, Enable: true,
	}); err != nil {
		t.Fatalf("Add proxy rule: %v", err)
	}
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: banned.Id, Action: model.ActionBlock, Priority: 99, Enable: true,
	}); err != nil {
		t.Fatalf("Add block rule: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 3 {
		t.Fatalf("rule count = %d, want 3", len(rules))
	}
	if rules[1]["outboundTag"] != model.BlockOutboundTag {
		t.Errorf("rules[1] = %v, want the block rule (block must outrank proxy)", rules[1])
	}
	if rules[2]["outboundTag"] != node.Tag {
		t.Errorf("rules[2] = %v, want the proxy rule", rules[2])
	}
}

func TestInjectGlobalRuleOmitsInboundTag(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		InboundId: 0, DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	generated := rules[len(rules)-1]
	if _, exists := generated["inboundTag"]; exists {
		t.Errorf("global rule must not carry inboundTag, got %v", generated["inboundTag"])
	}
}

func TestInjectPerInboundRuleUsesCurrentTag(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		InboundId: in.Id, DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	generated := decodeRules(t, cfg)[1]
	tags, ok := generated["inboundTag"].([]any)
	if !ok || len(tags) != 1 || tags[0] != in.Tag {
		t.Errorf("inboundTag = %v, want [%s]", generated["inboundTag"], in.Tag)
	}
}

// 这是最关键的一条：生成必须逐字节稳定，否则 Config.Equals 恒为 false，
// 那个 10 秒的 cron 会不停重启 xray。
func TestInjectIsDeterministic(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	node, _ := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	rs := RoutingRuleService{}
	for i := 0; i < 5; i++ {
		if err := rs.Add(&model.RoutingRule{
			DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	first := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(first); err != nil {
		t.Fatalf("Inject #1: %v", err)
	}
	second := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(second); err != nil {
		t.Fatalf("Inject #2: %v", err)
	}
	if string(first.OutboundConfigs) != string(second.OutboundConfigs) {
		t.Error("outbounds are not byte-identical across two runs")
	}
	if string(first.RouterConfig) != string(second.RouterConfig) {
		t.Error("routing is not byte-identical across two runs")
	}
	if !first.Equals(second) {
		t.Error("Config.Equals must report the two generated configs as equal")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run TestInject -v`
Expected: 编译失败，`undefined: RoutingInjector`

- [ ] **Step 3: 实现注入器**

创建 `web/service/routing_inject.go`：

```go
package service

import (
	"encoding/json"

	"a-ui/database/model"
	"a-ui/util/json_util"
	"a-ui/xray"
)

// RoutingInjector 把数据库里的出站节点与分流规则增量注入到 xray 配置中。
// 用户手写的 xrayTemplateConfig 原样保留，生成内容一律追加在末尾：
//   - 出站追加到末尾，模板里的 freedom 才能继续当 xray 的默认出站
//   - 规则追加到末尾，模板里屏蔽私网/BT 的安全规则才能保持更高优先级
type RoutingInjector struct {
	domainGroupService DomainGroupService
	outboundService    OutboundNodeService
	ruleService        RoutingRuleService
	inboundService     InboundService
}

func (s *RoutingInjector) Inject(cfg *xray.Config) error {
	outbounds, err := s.buildOutbounds(cfg.OutboundConfigs)
	if err != nil {
		return err
	}
	encodedOutbounds, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encodedOutbounds)

	blockRules, proxyRules, err := s.buildRules()
	if err != nil {
		return err
	}

	routing := map[string]any{}
	if len(cfg.RouterConfig) > 0 {
		if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
			return err
		}
	}
	rules, _ := routing["rules"].([]any)
	if rules == nil {
		rules = make([]any, 0)
	}
	rules = append(rules, blockRules...)
	rules = append(rules, proxyRules...)
	routing["rules"] = rules

	encodedRouting, err := json.Marshal(routing)
	if err != nil {
		return err
	}
	cfg.RouterConfig = json_util.RawMessage(encodedRouting)
	return nil
}

func (s *RoutingInjector) buildOutbounds(existing json_util.RawMessage) ([]any, error) {
	outbounds := make([]any, 0)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &outbounds); err != nil {
			return nil, err
		}
	}

	nodes, err := s.outboundService.GetEnabled()
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		var ob map[string]any
		if err := json.Unmarshal([]byte(node.Config), &ob); err != nil {
			// 单个节点配置损坏时跳过，不能让整份配置生成失败
			continue
		}
		ob["tag"] = node.Tag
		outbounds = append(outbounds, ob)
	}

	// 黑洞出站始终注入，不复用模板里的 blocked——用户可能把它删掉，
	// 而 xray 对悬空 outboundTag 不报错，block 规则会静默变成直连。
	outbounds = append(outbounds, map[string]any{
		"tag":      model.BlockOutboundTag,
		"protocol": "blackhole",
		"settings": map[string]any{},
	})
	return outbounds, nil
}

func (s *RoutingInjector) buildRules() ([]any, []any, error) {
	rules, err := s.ruleService.GetEnabled()
	if err != nil {
		return nil, nil, err
	}
	if len(rules) == 0 {
		return nil, nil, nil
	}

	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, nil, err
	}
	inboundTagById := make(map[int]string, len(inbounds))
	for _, in := range inbounds {
		if in.Enable {
			inboundTagById[in.Id] = in.Tag
		}
	}

	nodes, err := s.outboundService.GetEnabled()
	if err != nil {
		return nil, nil, err
	}
	outboundTagById := make(map[int]string, len(nodes))
	for _, node := range nodes {
		outboundTagById[node.Id] = node.Tag
	}

	blockRules := make([]any, 0)
	proxyRules := make([]any, 0)
	for _, rule := range rules {
		generated, isBlock := s.buildRule(rule, inboundTagById, outboundTagById)
		if generated == nil {
			continue
		}
		if isBlock {
			blockRules = append(blockRules, generated)
		} else {
			proxyRules = append(proxyRules, generated)
		}
	}
	return blockRules, proxyRules, nil
}

// buildRule 返回 nil 表示这条规则条件残缺，必须整条丢弃。
// 绝不能退而求其次生成一条缺少 domain 的规则：xray 把缺失的条件视为
// 「不限制」，那样的规则会劫持该入站的全部流量，且不会有任何报错。
func (s *RoutingInjector) buildRule(
	rule *model.RoutingRule,
	inboundTagById map[int]string,
	outboundTagById map[int]string,
) (map[string]any, bool) {
	group, err := s.domainGroupService.Get(rule.DomainGroupId)
	if err != nil {
		return nil, false
	}
	domains, err := DecodeDomains(group.Domains)
	if err != nil || len(domains) == 0 {
		return nil, false
	}

	generated := map[string]any{
		"type":   "field",
		"domain": domains,
	}

	if rule.InboundId > 0 {
		tag, ok := inboundTagById[rule.InboundId]
		if !ok {
			return nil, false
		}
		generated["inboundTag"] = []string{tag}
	}

	switch rule.Action {
	case model.ActionBlock:
		generated["outboundTag"] = model.BlockOutboundTag
		return generated, true
	case model.ActionProxy:
		tag, ok := outboundTagById[rule.OutboundId]
		if !ok {
			return nil, false
		}
		generated["outboundTag"] = tag
		return generated, false
	default:
		return nil, false
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./web/service/ -run TestInject -v`
Expected: 7 个测试全部 PASS

- [ ] **Step 5: 接进 GetXrayConfig**

在 `web/service/xray.go` 的 `XrayService` 结构体里加一个字段：

```go
type XrayService struct {
	inboundService  InboundService
	settingService  SettingService
	routingInjector RoutingInjector
}
```

把 `GetXrayConfig` 结尾的

```go
	return xrayConfig, nil
}
```

改成

```go
	if err := s.routingInjector.Inject(xrayConfig); err != nil {
		return nil, err
	}

	return xrayConfig, nil
}
```

- [ ] **Step 6: 确认整体编译与既有行为不变**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全部通过

- [ ] **Step 7: 用真实 xray 校验生成的配置**

启动面板（`XUI_DEBUG=true go run main.go`），等 10 秒让它写出 `bin/config.json`，然后：

```bash
./bin/xray-darwin-arm64 run -test -c bin/config.json
```

Expected: `Configuration OK.`（此时库里还没有任何规则，验证的是空数据下注入不破坏原配置）

- [ ] **Step 8: Commit**

```bash
git add web/service/routing_inject.go web/service/routing_inject_test.go web/service/xray.go
git commit -m "feat: 把出站节点与分流规则注入 xray 配置"
```

---

### Task 8: HTTP 接口

**Files:**
- Create: `web/controller/routing.go`
- Modify: `web/controller/xui.go:19-30`（`initRouter` 与结构体字段）
- Modify: `web/html/xui/common_sider.html:6-10`（菜单项）

**Interfaces:**
- Consumes: Task 4/5/6 的三个 service；现有的 `jsonMsg`、`jsonObj`、`html` 辅助函数
- Produces: `controller.NewRoutingController(g *gin.RouterGroup) *RoutingController`；13 个 POST 接口（见下）；`GET /xui/routing` 页面路由

- [ ] **Step 1: 写 controller**

创建 `web/controller/routing.go`：

```go
package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"a-ui/database/model"
	"a-ui/web/service"
)

type addOutboundForm struct {
	Remark string `json:"remark" form:"remark"`
	Link   string `json:"link" form:"link"`
	Config string `json:"config" form:"config"`
}

type domainGroupForm struct {
	Id      int    `json:"id" form:"id"`
	Remark  string `json:"remark" form:"remark"`
	Domains string `json:"domains" form:"domains"`
}

type RoutingController struct {
	domainGroupService service.DomainGroupService
	outboundService    service.OutboundNodeService
	ruleService        service.RoutingRuleService
	xrayService        service.XrayService
}

func NewRoutingController(g *gin.RouterGroup) *RoutingController {
	a := &RoutingController{}
	a.initRouter(g)
	return a
}

func (a *RoutingController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/routing")

	dg := g.Group("/domain-group")
	dg.POST("/list", a.listDomainGroups)
	dg.POST("/add", a.addDomainGroup)
	dg.POST("/update/:id", a.updateDomainGroup)
	dg.POST("/del/:id", a.delDomainGroup)

	ob := g.Group("/outbound")
	ob.POST("/list", a.listOutbounds)
	ob.POST("/add", a.addOutbound)
	ob.POST("/update/:id", a.updateOutbound)
	ob.POST("/del/:id", a.delOutbound)

	rl := g.Group("/rule")
	rl.POST("/list", a.listRules)
	rl.POST("/add", a.addRule)
	rl.POST("/update/:id", a.updateRule)
	rl.POST("/del/:id", a.delRule)
}

// 域名组

func (a *RoutingController) listDomainGroups(c *gin.Context) {
	groups, err := a.domainGroupService.GetAll()
	if err != nil {
		jsonMsg(c, "获取域名组", err)
		return
	}
	jsonObj(c, groups, nil)
}

// encodeDomainsFromForm 把 textarea 原文校验并转成入库格式。
func encodeDomainsFromForm(raw string) (string, error) {
	list, err := service.ParseDomains(raw)
	if err != nil {
		return "", err
	}
	return service.EncodeDomains(list)
}

func (a *RoutingController) addDomainGroup(c *gin.Context) {
	form := &domainGroupForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "添加域名组", err)
		return
	}
	encoded, err := encodeDomainsFromForm(form.Domains)
	if err != nil {
		jsonMsg(c, "添加域名组", err)
		return
	}
	group := &model.DomainGroup{Remark: form.Remark, Domains: encoded}
	err = a.domainGroupService.Add(group)
	jsonMsg(c, "添加域名组", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) updateDomainGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改域名组", err)
		return
	}
	form := &domainGroupForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "修改域名组", err)
		return
	}
	encoded, err := encodeDomainsFromForm(form.Domains)
	if err != nil {
		jsonMsg(c, "修改域名组", err)
		return
	}
	err = a.domainGroupService.Update(&model.DomainGroup{Id: id, Remark: form.Remark, Domains: encoded})
	jsonMsg(c, "修改域名组", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) delDomainGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "删除域名组", err)
		return
	}
	err = a.domainGroupService.Del(id)
	jsonMsg(c, "删除域名组", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

// 出站节点

func (a *RoutingController) listOutbounds(c *gin.Context) {
	nodes, err := a.outboundService.GetAll()
	if err != nil {
		jsonMsg(c, "获取出站节点", err)
		return
	}
	jsonObj(c, nodes, nil)
}

func (a *RoutingController) addOutbound(c *gin.Context) {
	form := &addOutboundForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "添加出站节点", err)
		return
	}
	var err error
	if form.Link != "" {
		_, err = a.outboundService.AddFromLink(form.Link, form.Remark)
	} else {
		_, err = a.outboundService.AddFromJSON(form.Config, form.Remark)
	}
	jsonMsg(c, "添加出站节点", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) updateOutbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改出站节点", err)
		return
	}
	node := &model.OutboundNode{Id: id}
	if err := c.ShouldBind(node); err != nil {
		jsonMsg(c, "修改出站节点", err)
		return
	}
	err = a.outboundService.Update(node)
	jsonMsg(c, "修改出站节点", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) delOutbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "删除出站节点", err)
		return
	}
	err = a.outboundService.Del(id)
	jsonMsg(c, "删除出站节点", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

// 分流规则

func (a *RoutingController) listRules(c *gin.Context) {
	rules, err := a.ruleService.GetAll()
	if err != nil {
		jsonMsg(c, "获取分流规则", err)
		return
	}
	jsonObj(c, rules, nil)
}

func (a *RoutingController) addRule(c *gin.Context) {
	rule := &model.RoutingRule{}
	if err := c.ShouldBind(rule); err != nil {
		jsonMsg(c, "添加分流规则", err)
		return
	}
	err := a.ruleService.Add(rule)
	jsonMsg(c, "添加分流规则", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) updateRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改分流规则", err)
		return
	}
	rule := &model.RoutingRule{Id: id}
	if err := c.ShouldBind(rule); err != nil {
		jsonMsg(c, "修改分流规则", err)
		return
	}
	err = a.ruleService.Update(rule)
	jsonMsg(c, "修改分流规则", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) delRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "删除分流规则", err)
		return
	}
	err = a.ruleService.Del(id)
	jsonMsg(c, "删除分流规则", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}
```

- [ ] **Step 2: 挂上页面路由**

在 `web/controller/xui.go` 中，把结构体改成：

```go
type XUIController struct {
	BaseController

	inboundController *InboundController
	settingController *SettingController
	routingController *RoutingController
}
```

在 `initRouter` 中，把

```go
	g.GET("/setting", a.setting)

	a.inboundController = NewInboundController(g)
	a.settingController = NewSettingController(g)
```

改成

```go
	g.GET("/setting", a.setting)
	g.GET("/routing", a.routing)

	a.inboundController = NewInboundController(g)
	a.settingController = NewSettingController(g)
	a.routingController = NewRoutingController(g)
```

并在文件末尾追加：

```go
func (a *XUIController) routing(c *gin.Context) {
	html(c, "routing.html", "分流管理", nil)
}
```

- [ ] **Step 3: 加侧边栏菜单**

在 `web/html/xui/common_sider.html` 中，把

```html
<a-menu-item key="{{ .base_path }}xui/setting">
    <a-icon type="setting"></a-icon>
    <span>面板设置</span>
</a-menu-item>
```

改成

```html
<a-menu-item key="{{ .base_path }}xui/routing">
    <a-icon type="fork"></a-icon>
    <span>分流管理</span>
</a-menu-item>
<a-menu-item key="{{ .base_path }}xui/setting">
    <a-icon type="setting"></a-icon>
    <span>面板设置</span>
</a-menu-item>
```

- [ ] **Step 4: 确认编译通过**

Run: `go build ./... && go vet ./...`
Expected: 无输出

此时访问 `/xui/routing` 会因为模板缺失而报错，Task 9 补上。

- [ ] **Step 5: Commit**

```bash
git add web/controller/routing.go web/controller/xui.go web/html/xui/common_sider.html
git commit -m "feat: 分流管理的 HTTP 接口与页面入口"
```

---

### Task 9: 前端页面

**Files:**
- Create: `web/html/xui/routing.html`
- Create: `web/assets/js/model/routing.js`

**Interfaces:**
- Consumes: Task 8 的 13 个接口；现有的 `web/html/common/head.html`、`js.html`、`web/html/xui/common_sider.html` 模板；全局 `basePath`
- Produces: 无（页面是叶子节点）

参照 `web/html/xui/setting.html` 的既有写法：Go 模板 + Vue 2 + ant-design-vue 1.7.2，Vue 插值分隔符是 `[[ ]]`（`{{ }}` 归 Go 模板用）。

- [ ] **Step 1: 写前端数据模型**

创建 `web/assets/js/model/routing.js`：

```javascript
const RULE_ACTION = {
    PROXY: "proxy",
    BLOCK: "block",
};

const ACTION_LABEL = {
    proxy: "走节点",
    block: "阻断",
};

class DomainGroup {
    constructor(id = 0, remark = "", domains = []) {
        this.id = id;
        this.remark = remark;
        this.domains = domains;
    }

    static fromJson(json = {}) {
        let domains = [];
        try {
            domains = JSON.parse(json.domains || "[]");
        } catch (e) {
            domains = [];
        }
        return new DomainGroup(json.id, json.remark, domains);
    }

    get text() {
        return this.domains.join("\n");
    }
}

class OutboundNode {
    constructor(id = 0, tag = "", remark = "", protocol = "", config = "", enable = true) {
        this.id = id;
        this.tag = tag;
        this.remark = remark;
        this.protocol = protocol;
        this.config = config;
        this.enable = enable;
    }

    static fromJson(json = {}) {
        return new OutboundNode(json.id, json.tag, json.remark, json.protocol, json.config, json.enable);
    }
}

class RoutingRule {
    constructor(id = 0, remark = "", inboundId = 0, domainGroupId = 0,
                action = RULE_ACTION.PROXY, outboundId = 0, priority = 0, enable = true) {
        this.id = id;
        this.remark = remark;
        this.inboundId = inboundId;
        this.domainGroupId = domainGroupId;
        this.action = action;
        this.outboundId = outboundId;
        this.priority = priority;
        this.enable = enable;
    }

    static fromJson(json = {}) {
        return new RoutingRule(json.id, json.remark, json.inboundId, json.domainGroupId,
            json.action, json.outboundId, json.priority, json.enable);
    }
}
```

- [ ] **Step 2: 写页面**

创建 `web/html/xui/routing.html`：

```html
{{template "head" .}}
<body>
<a-layout id="app" v-cloak>
    {{template "commonSider" .}}
    <a-layout id="content-layout">
        <a-layout-content>
            <a-spin :spinning="spinning" :delay="500" tip="加载中">
                <a-tabs default-active-key="1">

                    <a-tab-pane key="1" tab="域名组">
                        <a-button type="primary" @click="openGroup()">添加域名组</a-button>
                        <a-table :columns="groupColumns" :data-source="groups"
                                 :row-key="r => r.id" :pagination="false" style="margin-top: 16px;">
                            <template slot="domains" slot-scope="text, group">
                                <a-tag v-for="d in group.domains" :key="d">[[ d ]]</a-tag>
                            </template>
                            <template slot="action" slot-scope="text, group">
                                <a-icon type="edit" @click="openGroup(group)"></a-icon>
                                <a-divider type="vertical"></a-divider>
                                <a-icon type="delete" style="color: #ff4d4f;"
                                        @click="delGroup(group)"></a-icon>
                            </template>
                        </a-table>
                    </a-tab-pane>

                    <a-tab-pane key="2" tab="出站节点">
                        <a-button type="primary" @click="openNode()">添加出站节点</a-button>
                        <a-table :columns="nodeColumns" :data-source="nodes"
                                 :row-key="r => r.id" :pagination="false" style="margin-top: 16px;">
                            <template slot="enable" slot-scope="text, node">
                                <a-switch :checked="node.enable"
                                          @change="toggleNode(node)"></a-switch>
                            </template>
                            <template slot="action" slot-scope="text, node">
                                <a-icon type="delete" style="color: #ff4d4f;"
                                        @click="delNode(node)"></a-icon>
                            </template>
                        </a-table>
                    </a-tab-pane>

                    <a-tab-pane key="3" tab="分流规则">
                        <a-button type="primary" @click="openRule()">添加分流规则</a-button>
                        <a-table :columns="ruleColumns" :data-source="rules"
                                 :row-key="r => r.id" :pagination="false" style="margin-top: 16px;">
                            <template slot="inbound" slot-scope="text, rule">
                                [[ inboundName(rule.inboundId) ]]
                            </template>
                            <template slot="group" slot-scope="text, rule">
                                [[ groupName(rule.domainGroupId) ]]
                            </template>
                            <template slot="target" slot-scope="text, rule">
                                <a-tag v-if="rule.action === 'block'" color="red">阻断</a-tag>
                                <a-tag v-else color="blue">[[ nodeName(rule.outboundId) ]]</a-tag>
                            </template>
                            <template slot="action" slot-scope="text, rule">
                                <a-icon type="edit" @click="openRule(rule)"></a-icon>
                                <a-divider type="vertical"></a-divider>
                                <a-icon type="delete" style="color: #ff4d4f;"
                                        @click="delRule(rule)"></a-icon>
                            </template>
                        </a-table>
                    </a-tab-pane>

                </a-tabs>
            </a-spin>
        </a-layout-content>
    </a-layout>
</a-layout>

<a-modal v-model="groupModal.visible" title="域名组"
         @ok="saveGroup" ok-text="保存" cancel-text="取消">
    <a-form layout="vertical">
        <a-form-item label="备注">
            <a-input v-model="groupModal.remark"></a-input>
        </a-form-item>
        <a-form-item label="域名（一行一条）">
            <a-input type="textarea" :rows="8" v-model="groupModal.domains"
                     placeholder="domain:openai.com&#10;geosite:openai&#10;full:chat.openai.com"></a-input>
        </a-form-item>
    </a-form>
</a-modal>

<a-modal v-model="nodeModal.visible" title="出站节点"
         @ok="saveNode" ok-text="保存" cancel-text="取消">
    <a-form layout="vertical">
        <a-form-item label="备注">
            <a-input v-model="nodeModal.remark"></a-input>
        </a-form-item>
        <a-form-item label="分享链接">
            <a-input type="textarea" :rows="4" v-model="nodeModal.link"
                     placeholder="vmess:// vless:// trojan:// ss:// socks:// hysteria2://"></a-input>
        </a-form-item>
        <a-form-item label="或直接粘贴 outbound JSON（高级）">
            <a-input type="textarea" :rows="6" v-model="nodeModal.config"></a-input>
        </a-form-item>
    </a-form>
</a-modal>

<a-modal v-model="ruleModal.visible" title="分流规则"
         @ok="saveRule" ok-text="保存" cancel-text="取消">
    <a-form layout="vertical">
        <a-form-item label="备注">
            <a-input v-model="ruleModal.rule.remark"></a-input>
        </a-form-item>
        <a-form-item label="用户（入站）">
            <a-select v-model="ruleModal.rule.inboundId" style="width: 100%;">
                <a-select-option :value="0">所有用户</a-select-option>
                <a-select-option v-for="i in inbounds" :key="i.id" :value="i.id">
                    [[ i.remark || i.tag ]]
                </a-select-option>
            </a-select>
        </a-form-item>
        <a-form-item label="域名组">
            <a-select v-model="ruleModal.rule.domainGroupId" style="width: 100%;">
                <a-select-option v-for="g in groups" :key="g.id" :value="g.id">
                    [[ g.remark ]]
                </a-select-option>
            </a-select>
        </a-form-item>
        <a-form-item label="动作">
            <a-radio-group v-model="ruleModal.rule.action">
                <a-radio value="proxy">走节点</a-radio>
                <a-radio value="block">阻断（黑洞）</a-radio>
            </a-radio-group>
        </a-form-item>
        <a-form-item label="出站节点" v-if="ruleModal.rule.action === 'proxy'">
            <a-select v-model="ruleModal.rule.outboundId" style="width: 100%;">
                <a-select-option v-for="n in nodes" :key="n.id" :value="n.id">
                    [[ n.remark ]] ([[ n.protocol ]])
                </a-select-option>
            </a-select>
        </a-form-item>
        <a-form-item label="优先级（数字越小越先匹配）">
            <a-input-number v-model="ruleModal.rule.priority" :min="0"></a-input-number>
        </a-form-item>
    </a-form>
</a-modal>

{{template "js" .}}
<script src="{{ .base_path }}assets/js/model/routing.js?{{ .cur_ver }}"></script>
<script>
    const app = new Vue({
        delimiters: ['[[', ']]'],
        el: '#app',
        data: {
            spinning: false,
            groups: [],
            nodes: [],
            rules: [],
            inbounds: [],
            groupColumns: [
                { title: '备注', dataIndex: 'remark', width: 160 },
                { title: '域名', scopedSlots: { customRender: 'domains' } },
                { title: '操作', width: 100, scopedSlots: { customRender: 'action' } },
            ],
            nodeColumns: [
                { title: '备注', dataIndex: 'remark', width: 160 },
                { title: 'tag', dataIndex: 'tag', width: 160 },
                { title: '协议', dataIndex: 'protocol', width: 100 },
                { title: '启用', width: 80, scopedSlots: { customRender: 'enable' } },
                { title: '操作', width: 80, scopedSlots: { customRender: 'action' } },
            ],
            ruleColumns: [
                { title: '备注', dataIndex: 'remark', width: 140 },
                { title: '用户', scopedSlots: { customRender: 'inbound' }, width: 140 },
                { title: '域名组', scopedSlots: { customRender: 'group' }, width: 140 },
                { title: '去向', scopedSlots: { customRender: 'target' }, width: 140 },
                { title: '优先级', dataIndex: 'priority', width: 80 },
                { title: '操作', width: 100, scopedSlots: { customRender: 'action' } },
            ],
            groupModal: { visible: false, id: 0, remark: '', domains: '' },
            nodeModal: { visible: false, remark: '', link: '', config: '' },
            ruleModal: { visible: false, rule: new RoutingRule() },
        },
        methods: {
            async post(url, data) {
                this.spinning = true;
                try {
                    const resp = await axios.post(url, data);
                    const msg = resp.data;
                    if (msg.success) {
                        if (msg.msg) this.$message.success(msg.msg);
                    } else {
                        this.$message.error(msg.msg);
                    }
                    return msg;
                } finally {
                    this.spinning = false;
                }
            },
            async loadAll() {
                const [g, n, r, i] = await Promise.all([
                    axios.post('xui/routing/domain-group/list'),
                    axios.post('xui/routing/outbound/list'),
                    axios.post('xui/routing/rule/list'),
                    axios.post('xui/inbound/list'),
                ]);
                this.groups = (g.data.obj || []).map(DomainGroup.fromJson);
                this.nodes = (n.data.obj || []).map(OutboundNode.fromJson);
                this.rules = (r.data.obj || []).map(RoutingRule.fromJson);
                this.inbounds = i.data.obj || [];
            },
            groupName(id) {
                const g = this.groups.find(x => x.id === id);
                return g ? g.remark : '(已删除)';
            },
            nodeName(id) {
                const n = this.nodes.find(x => x.id === id);
                return n ? n.remark : '(已删除)';
            },
            inboundName(id) {
                if (!id) return '所有用户';
                const i = this.inbounds.find(x => x.id === id);
                return i ? (i.remark || i.tag) : '(已删除)';
            },
            openGroup(group) {
                this.groupModal.id = group ? group.id : 0;
                this.groupModal.remark = group ? group.remark : '';
                this.groupModal.domains = group ? group.text : '';
                this.groupModal.visible = true;
            },
            async saveGroup() {
                const data = { remark: this.groupModal.remark, domains: this.groupModal.domains };
                const url = this.groupModal.id
                    ? 'xui/routing/domain-group/update/' + this.groupModal.id
                    : 'xui/routing/domain-group/add';
                const msg = await this.post(url, data);
                if (msg.success) {
                    this.groupModal.visible = false;
                    await this.loadAll();
                }
            },
            async delGroup(group) {
                const msg = await this.post('xui/routing/domain-group/del/' + group.id);
                if (msg.success) await this.loadAll();
            },
            openNode() {
                this.nodeModal.remark = '';
                this.nodeModal.link = '';
                this.nodeModal.config = '';
                this.nodeModal.visible = true;
            },
            async saveNode() {
                const msg = await this.post('xui/routing/outbound/add', {
                    remark: this.nodeModal.remark,
                    link: this.nodeModal.link.trim(),
                    config: this.nodeModal.config.trim(),
                });
                if (msg.success) {
                    this.nodeModal.visible = false;
                    await this.loadAll();
                }
            },
            async toggleNode(node) {
                const msg = await this.post('xui/routing/outbound/update/' + node.id, {
                    remark: node.remark, enable: !node.enable, config: '',
                });
                if (msg.success) await this.loadAll();
            },
            async delNode(node) {
                const msg = await this.post('xui/routing/outbound/del/' + node.id);
                if (msg.success) await this.loadAll();
            },
            openRule(rule) {
                this.ruleModal.rule = rule
                    ? RoutingRule.fromJson(JSON.parse(JSON.stringify(rule)))
                    : new RoutingRule();
                this.ruleModal.visible = true;
            },
            async saveRule() {
                const r = this.ruleModal.rule;
                const url = r.id ? 'xui/routing/rule/update/' + r.id : 'xui/routing/rule/add';
                const msg = await this.post(url, r);
                if (msg.success) {
                    this.ruleModal.visible = false;
                    await this.loadAll();
                }
            },
            async delRule(rule) {
                const msg = await this.post('xui/routing/rule/del/' + rule.id);
                if (msg.success) await this.loadAll();
            },
        },
        async mounted() {
            this.spinning = true;
            try {
                await this.loadAll();
            } finally {
                this.spinning = false;
            }
        },
    });
</script>
</body>
```

- [ ] **Step 3: 启动面板检查页面能打开**

```bash
XUI_DEBUG=true go run main.go
```

浏览器打开 `http://localhost:54321/xui/routing`。

Expected: 侧边栏出现「分流管理」，页面显示三个 tab，无 JS 报错（开 DevTools Console 确认）。

**注意**：改了 `web/assets/js/**` 后必须硬刷新（Cmd+Shift+R）或在 DevTools 里勾 Disable cache——`web/web.go` 那个加 `Cache-Control: max-age=31536000` 的中间件不区分 debug 模式，而 `?cur_ver` 版本号不变，否则会一直命中旧缓存。

- [ ] **Step 4: Commit**

```bash
git add web/html/xui/routing.html web/assets/js/model/routing.js
git commit -m "feat: 分流管理页面"
```

---

### Task 10: 保存前用真实 xray 校验（spec §5.4.2）

用户在 JSON 高级模式粘一段结构错误的 outbound，或者在域名组里写一个不存在的 `geosite:` 类别、非法的 `regexp:`，都会让整份配置变成非法配置——xray 起不来，**所有用户一起断网**。这个任务在落库之前就把这类输入挡回去。

校验策略是 **fail open**：只有当 xray 明确判定配置非法时才拒绝保存。二进制不存在、命令行参数不被老版本识别、执行超时等情况一律放行并记日志。校验是辅助手段，绝不能因为它自身故障就把用户锁在门外。

**Files:**
- Create: `web/service/routing_validate.go`
- Create: `web/service/routing_validate_test.go`
- Modify: `web/service/routing_outbound.go`（`persist` 内，`allocTag` 之前）
- Modify: `web/controller/routing.go`（`encodeDomainsFromForm` 内）

**Interfaces:**
- Consumes: `xray.GetBinaryPath()`；Task 5 的 `persist`
- Produces: `service.ValidateOutbound(ob map[string]any) error`、`service.ValidateDomains(domains []string) error`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/routing_validate_test.go`：

```go
package service

import (
	"os"
	"testing"

	"a-ui/xray"
)

// 校验依赖真实的 xray 二进制，没有就跳过——CI 里可能没有。
func requireXrayBinary(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(xray.GetBinaryPath()); err != nil {
		t.Skipf("xray binary not available at %s", xray.GetBinaryPath())
	}
}

func TestValidateOutboundAcceptsValidSocks(t *testing.T) {
	requireXrayBinary(t)
	ob := map[string]any{
		"tag":      "a-ui-probe",
		"protocol": "socks",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": "1.2.3.4", "port": 1080}},
		},
	}
	if err := ValidateOutbound(ob); err != nil {
		t.Errorf("valid socks outbound was rejected: %v", err)
	}
}

func TestValidateOutboundRejectsUnknownProtocol(t *testing.T) {
	requireXrayBinary(t)
	ob := map[string]any{
		"tag":      "a-ui-probe",
		"protocol": "definitely-not-a-protocol",
		"settings": map[string]any{},
	}
	if err := ValidateOutbound(ob); err == nil {
		t.Error("expected an unknown protocol to be rejected")
	}
}

func TestValidateDomainsAcceptsNativeSyntax(t *testing.T) {
	requireXrayBinary(t)
	if err := ValidateDomains([]string{"domain:openai.com", "geosite:openai", "full:chat.openai.com"}); err != nil {
		t.Errorf("valid domains were rejected: %v", err)
	}
}

func TestValidateDomainsRejectsUnknownGeositeCategory(t *testing.T) {
	requireXrayBinary(t)
	if err := ValidateDomains([]string{"geosite:definitely-not-a-category"}); err == nil {
		t.Error("expected an unknown geosite category to be rejected")
	}
}

func TestValidateDomainsRejectsBadRegexp(t *testing.T) {
	requireXrayBinary(t)
	if err := ValidateDomains([]string{`regexp:([a-z`}); err == nil {
		t.Error("expected a malformed regexp to be rejected")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run TestValidate -v`
Expected: 编译失败，`undefined: ValidateOutbound`

- [ ] **Step 3: 实现校验器**

创建 `web/service/routing_validate.go`：

```go
package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/xray"
)

// runXrayTest 把一份配置交给真实的 xray 做语法与语义校验。
//
// 采用 fail open：只有 xray 明确判定配置非法时才返回错误。二进制缺失、
// 命令行参数不被老版本识别、超时等情况一律返回 nil 并记日志——校验器
// 自身的故障绝不能变成用户无法保存配置的门禁。
func runXrayTest(cfg map[string]any) error {
	binaryPath := xray.GetBinaryPath()
	if _, err := os.Stat(binaryPath); err != nil {
		logger.Debug("skip config validation, xray binary not found:", err)
		return nil
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "a-ui-validate-*.json")
	if err != nil {
		logger.Warning("skip config validation, cannot create temp file:", err)
		return nil
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		logger.Warning("skip config validation, cannot write temp file:", err)
		return nil
	}
	file.Close()

	cmd := exec.Command(binaryPath, "run", "-test", "-c", file.Name())
	done := make(chan struct{})
	var output []byte
	var runErr error
	go func() {
		output, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		logger.Warning("skip config validation, xray -test timed out")
		return nil
	}

	text := string(output)
	if runErr == nil || strings.Contains(text, "Configuration OK") {
		return nil
	}
	// 老版本 xray 可能不认 "run -test" 这套参数，此时不是配置的问题。
	if strings.Contains(text, "unknown command") || strings.Contains(text, "flag provided but not defined") {
		logger.Warning("skip config validation, xray does not support 'run -test':", firstLine(text))
		return nil
	}
	return common.NewError("xray 校验未通过:", lastMeaningfulLine(text))
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return s
}

// lastMeaningfulLine 取最后一行非空、非版权横幅的输出，那里才是真正的报错。
func lastMeaningfulLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" ||
			strings.HasPrefix(line, "Xray ") ||
			strings.HasPrefix(line, "A unified platform") {
			continue
		}
		return line
	}
	return firstLine(s)
}

// ValidateOutbound 用一份最小配置把待验证的 outbound 包起来送去校验。
// 在落库之前调用，因此不需要事务回滚。
func ValidateOutbound(ob map[string]any) error {
	return runXrayTest(map[string]any{
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "settings": map[string]any{}},
			ob,
		},
	})
}

// ValidateDomains 校验域名列表，能抓出不存在的 geosite 类别与非法正则。
func ValidateDomains(domains []string) error {
	return runXrayTest(map[string]any{
		"outbounds": []any{
			map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}},
		},
		"routing": map[string]any{
			"rules": []any{
				map[string]any{"type": "field", "domain": domains, "outboundTag": "direct"},
			},
		},
	})
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./web/service/ -run TestValidate -v`
Expected: 5 个测试全部 PASS（若本机没有 `bin/xray-darwin-arm64` 则全部 SKIP，此时先按 Task 2 Step 6 的方式准备好二进制再跑）

- [ ] **Step 5: 接进出站节点保存路径**

修改 `web/service/routing_outbound.go` 的 `persist`，在 `allocTag` 之前插入校验：

```go
func (s *OutboundNodeService) persist(ob map[string]any, protocol, remark string) (*model.OutboundNode, error) {
	if err := ValidateOutbound(ob); err != nil {
		return nil, err
	}
	tag, err := s.allocTag(remark)
	if err != nil {
		return nil, err
	}
	ob["tag"] = tag
	// ...以下不变
```

- [ ] **Step 6: 接进域名组保存路径**

修改 `web/controller/routing.go` 的 `encodeDomainsFromForm`：

```go
func encodeDomainsFromForm(raw string) (string, error) {
	list, err := service.ParseDomains(raw)
	if err != nil {
		return "", err
	}
	if err := service.ValidateDomains(list); err != nil {
		return "", err
	}
	return service.EncodeDomains(list)
}
```

- [ ] **Step 7: 回归**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全部通过。注意 Task 5 的 `TestAddFromLink*` 现在会真的调用 xray 校验，若本机没有二进制会走 fail open 分支，仍应 PASS。

- [ ] **Step 8: Commit**

```bash
git add web/service/routing_validate.go web/service/routing_validate_test.go web/service/routing_outbound.go web/controller/routing.go
git commit -m "feat: 保存前用真实 xray 校验出站与域名配置"
```

---

### Task 11: 链接解析预览与 sniffing 警告（spec §5.4.1、§8）

两件事：让用户在保存前看到链接被解析成了什么；以及在规则列表里标出「这条规则挂在一个没开 sniffing 的入站上，域名匹配根本不会命中」。

**Files:**
- Modify: `web/controller/routing.go`（`initRouter` 的 outbound 组，新增一个接口）
- Modify: `web/html/xui/routing.html`（出站弹窗加解析按钮；规则表格加警告列）

**Interfaces:**
- Consumes: Task 1/2 的 `link.ParseLink`；Task 9 的页面
- Produces: `POST /xui/routing/outbound/parse`，入参 `{link}`，返回解析后的 outbound JSON 字符串

- [ ] **Step 1: 加解析预览接口**

在 `web/controller/routing.go` 的 import 中补上 `"encoding/json"` 与 `"a-ui/util/link"`，然后在 `initRouter` 的 outbound 组里追加一行：

```go
	ob.POST("/del/:id", a.delOutbound)
	ob.POST("/parse", a.parseOutboundLink)
```

并在「出站节点」一节的末尾追加：

```go
// parseOutboundLink 只解析不落库，供前端预览。
func (a *RoutingController) parseOutboundLink(c *gin.Context) {
	form := &addOutboundForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "解析链接", err)
		return
	}
	result, err := link.ParseLink(form.Link)
	if err != nil {
		jsonMsg(c, "解析链接", err)
		return
	}
	encoded, err := json.MarshalIndent(result.Outbound, "", "  ")
	if err != nil {
		jsonMsg(c, "解析链接", err)
		return
	}
	jsonObj(c, string(encoded), nil)
}
```

- [ ] **Step 2: 确认接口可用**

启动面板后：

```bash
curl -s -X POST http://localhost:54321/xui/routing/outbound/parse \
  -H 'Content-Type: application/json' -d '{"link":"socks5://1.2.3.4:1080"}'
```

Expected: 未登录时返回登录跳转；登录后（带 cookie）返回 `{"success":true,...,"obj":"{\n  \"protocol\": \"socks\"..."}`。用浏览器 DevTools 的 Console 执行 `axios.post('xui/routing/outbound/parse',{link:'socks5://1.2.3.4:1080'}).then(r=>console.log(r.data))` 更省事。

- [ ] **Step 3: 前端加解析按钮**

在 `web/html/xui/routing.html` 的出站弹窗中，把

```html
        <a-form-item label="或直接粘贴 outbound JSON（高级）">
            <a-input type="textarea" :rows="6" v-model="nodeModal.config"></a-input>
        </a-form-item>
```

改成

```html
        <a-form-item>
            <a-button @click="parseLink" :disabled="!nodeModal.link.trim()">解析预览</a-button>
        </a-form-item>
        <a-form-item label="或直接粘贴 outbound JSON（高级）">
            <a-input type="textarea" :rows="6" v-model="nodeModal.config"></a-input>
        </a-form-item>
```

在 `methods` 中追加：

```javascript
            async parseLink() {
                const msg = await this.post('xui/routing/outbound/parse', {
                    link: this.nodeModal.link.trim(),
                });
                if (msg.success) {
                    this.nodeModal.config = msg.obj;
                    this.$message.success('解析成功，可在下方核对后保存');
                }
            },
```

注意：解析结果填进 config 框后，`saveNode` 里 `link` 仍然非空，会走 `AddFromLink` 分支重新解析一遍——结果一致，无副作用。用户若手工改了 JSON 想让改动生效，需先清空链接框。这个取舍在 UI 上用 placeholder 说明即可，不必加额外状态。

- [ ] **Step 4: 前端加 sniffing 警告**

在 `ruleColumns` 的「去向」与「优先级」之间插入一列：

```javascript
                { title: '去向', scopedSlots: { customRender: 'target' }, width: 140 },
                { title: '', width: 40, scopedSlots: { customRender: 'warn' } },
                { title: '优先级', dataIndex: 'priority', width: 80 },
```

在规则表格里追加对应插槽：

```html
                            <template slot="warn" slot-scope="text, rule">
                                <a-tooltip v-if="sniffingOff(rule)"
                                           title="该入站未开启 sniffing（http/tls），xray 在路由阶段拿不到域名，这条域名规则不会命中">
                                    <a-icon type="warning" style="color: #faad14;"></a-icon>
                                </a-tooltip>
                            </template>
```

在 `methods` 中追加：

```javascript
            // 域名分流依赖 sniffing 拿到 SNI/Host。入站关掉 sniffing 或
            // destOverride 不含 http/tls 时，域名规则永远不会命中。
            sniffingOff(rule) {
                if (!rule.inboundId) return false;
                const inbound = this.inbounds.find(x => x.id === rule.inboundId);
                if (!inbound) return false;
                let sniffing;
                try {
                    sniffing = JSON.parse(inbound.sniffing || '{}');
                } catch (e) {
                    return true;
                }
                if (!sniffing.enabled) return true;
                const dest = sniffing.destOverride || [];
                return !dest.includes('http') && !dest.includes('tls');
            },
```

- [ ] **Step 5: 页面自测**

启动面板，硬刷新（Cmd+Shift+R）后：

1. 出站节点 → 添加，链接填 `socks5://1.2.3.4:1080`，点「解析预览」
   Expected: 下方 JSON 框出现 `"protocol": "socks"` 的完整配置
2. 入站列表里把某个入站的 sniffing 关掉，再回到分流规则页
   Expected: 引用该入站的规则那一行出现黄色警告图标，悬停显示原因

- [ ] **Step 6: Commit**

```bash
git add web/controller/routing.go web/html/xui/routing.html
git commit -m "feat: 链接解析预览与 sniffing 未开启警告"
```

---

### Task 12: 端到端验证

**Files:** 无（纯验证）

**Interfaces:**
- Consumes: 全部前序任务
- Produces: 无

- [ ] **Step 1: 跑全量自动化检查**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全部通过

- [ ] **Step 2: 造一份完整数据**

启动 `XUI_DEBUG=true go run main.go`，在面板里依次操作：

1. 入站列表建两个入站（端口 10001、10002），备注分别写「甲」「乙」
2. 分流管理 → 域名组 → 添加，备注「ChatGPT」，域名填 `geosite:openai` 和 `domain:chatgpt.com`（两行）
3. 域名组 → 添加，备注「违规域名」，域名填 `domain:doubleclick.net`
4. 出站节点 → 添加，备注「B 节点」，分享链接填 `socks5://1.2.3.4:1080`
5. 出站节点 → 添加，备注「C 节点」，分享链接填 `socks5://5.6.7.8:1080`
6. 分流规则 → 添加：用户「甲」× 域名组「ChatGPT」× 走节点「B 节点」
7. 分流规则 → 添加：用户「乙」× 域名组「ChatGPT」× 走节点「C 节点」
8. 分流规则 → 添加：用户「所有用户」× 域名组「违规域名」× 阻断

- [ ] **Step 3: 校验生成的配置**

等 10 秒让重启任务写出配置，然后：

```bash
./bin/xray-darwin-arm64 run -test -c bin/config.json
```

Expected: `Configuration OK.`

- [ ] **Step 4: 人工核对配置内容**

```bash
python3 -m json.tool bin/config.json | grep -A 6 '"rules"'
```

逐条确认：

- `a-ui-block` 出站存在，且 outbounds 数组第一个仍是 `freedom`
- 三条生成规则排在模板原有规则之后
- **阻断规则排在两条走节点规则之前**
- 阻断规则**没有** `inboundTag` 字段（全局生效）
- 两条走节点规则的 `inboundTag` 分别是 `inbound-10001`、`inbound-10002`，`outboundTag` 指向不同的 `a-ui-` 节点

- [ ] **Step 5: 验证引用完整性拦截**

在面板里尝试删除域名组「ChatGPT」。

Expected: 报错「该域名组仍被 2 条分流规则引用，请先删除这些规则」，删除被拒。

再尝试删除出站节点「B 节点」。

Expected: 报错「该出站节点仍被 1 条分流规则引用，请先删除这些规则」。

- [ ] **Step 6: 验证非法输入在落库前被拒（Task 10）**

在面板里依次尝试：

1. 域名组 → 添加，域名填 `geosite:definitely-not-a-category`
   Expected: 报错「xray 校验未通过: ...」，未落库
2. 域名组 → 添加，域名填 `regexp:([a-z`
   Expected: 同样被拒
3. 出站节点 → 添加，JSON 高级模式填 `{"protocol":"definitely-not-a-protocol","settings":{}}`
   Expected: 同样被拒

三次操作后回到列表页确认没有产生任何脏数据。

- [ ] **Step 7: 验证改端口后规则不失效**

把入站「甲」的端口从 10001 改成 10011，等 10 秒后重新检查配置：

```bash
python3 -m json.tool bin/config.json | grep -B 2 -A 4 'inbound-10011'
```

Expected: 对应规则的 `inboundTag` 自动变成 `inbound-10011`，规则依然存在——这验证了规则存 `InboundId` 而非 tag 的设计。

- [ ] **Step 8: 验证不会反复重启 xray**

保持面板运行 2 分钟，观察日志。

Expected: 日志中**没有**周期性的 `restart xray` —— 若每 10 秒出现一次，说明生成不确定，回到 Task 7 检查排序稳定性。

- [ ] **Step 9: 清理并提交**

```bash
rm -f /tmp/socks-check.json
git status   # 确认没有多余的临时文件
git add -A
git commit -m "test: 域名分流管理端到端验证通过"
```
