# 来源 IP 运营商列 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在入站列表展开行与访问日志弹窗里，对每个来源 IP 显示归一化后的运营商（含机房标识）。

**Architecture:** 两个离线库的源数据里 ISP 字段本来就有，只是解析时被丢弃。做法是把 `ipdb` 的紧凑格式从 v1 升到 v2（归属地多一个 ISP 字符串下标），`Parse` 同时认两个版本，两个源的解析器各放开一个字段并经同一个归一化函数落库，服务层与前端各加一个字段/一列。

**Tech Stack:** Go 1.27.0（CGO_ENABLED=1）、标准库、`golang.org/x/text`（纯真库 GBK 解码，已有依赖）、Vue 2 + ant-design-vue 服务端模板。

**Spec:** `docs/superpowers/specs/2026-09-07-ip-isp-column-design.md`

## Global Constraints

- **格式版本 v2，`Parse` 必须同时认 v1**：读到 v1 时 ISP 留空。违反这条会让升级后整个归属地列消失（不只是新列），且共享检测的省份判定一起失效。
- **归一化必须发生在落库前**，不能放到展示层：格式的归属地种类上限是 65536，纯真库不归一有 115,619 种取值，直接撞上限导致整次更新失败。
- **境外段只保留知名 IDC / 云厂商的 ISP，其余留空**：全量保留会让纯真库段数从 261,670 涨到 858,363（+228%）。
- **生成必须逐字节确定**：任何用 map 遍历产生顺序的写法都是错的。关键词匹配表用切片不用 map。
- **`CIDRsOfProvinces` 的输出在加 ISP 前后必须逐字节相同**：它的结果进 geo dat 的内容哈希，一变就会让那个 10 秒的 cron 反复重启 xray。
- 测试命令：`go test ./util/ipdb/... ./util/qqwry/... ./web/...`；提交前门禁 `make verify`。
- 项目文风：注释解释「为什么」和「不这样做会怎样」，不复述代码表面含义。所有面向管理员的文案是简体中文。

---

### Task 1: ISP 名称归一化

**Files:**
- Create: `util/ipdb/isp_name.go`
- Test: `util/ipdb/isp_name_test.go`

**Interfaces:**
- Consumes: 无（纯函数，无包内依赖）
- Produces: `func CanonicalISP(raw string, foreign bool) string` — Task 3（ip2region 解析）与 Task 4（纯真库解析）都调它。`foreign` 为 true 表示这条记录的国家不是「中国」。

- [ ] **Step 1: 写失败的测试**

创建 `util/ipdb/isp_name_test.go`：

```go
package ipdb

import "testing"

func TestCanonicalISPUnifiesChineseCarriers(t *testing.T) {
	// 两个源对同一家运营商的写法不同，必须收敛到同一个字符串。
	// 不收敛的话 Multi.Lookup 的分歧判定（按字符串相等）会把它们当成
	// 两个不同的结论，「存疑」标签会对几乎每个 IP 亮起。
	for _, c := range []struct{ raw, want string }{
		{"电信", "中国电信"},
		{"中国电信", "中国电信"},
		{"CHINANET", "中国电信"},
		{"联通", "中国联通"},
		{"中国联通", "中国联通"},
		{"网通", "中国联通"},
		{"移动", "中国移动"},
		{"铁通", "中国移动"},
		{"中移铁通", "中国移动"},
		{"广电网", "中国广电"},
		{"教育网", "教育网"},
		{"中国教育网", "教育网"},
	} {
		if got := CanonicalISP(c.raw, false); got != c.want {
			t.Errorf("CanonicalISP(%q, false) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestCanonicalISPStripsSubLabelsAndPlaceholders(t *testing.T) {
	// 纯真库把同一家运营商按接入点拆成上万种写法（"电信/新世纪网吧"），
	// 不截断会让归属地种类撞上 65536 的格式上限、整次更新失败。
	for _, c := range []struct{ raw, want string }{
		{"电信/新世纪网吧", "中国电信"},
		{"移动/全省通用", "中国移动"},
		{"联通/联通信息港", "中国联通"},
		{" CZ88.NET", ""},   // 纯真库的广告占位
		{"0", ""},           // ip2region 的未知占位
		{"  ", ""},
		{"", ""},
	} {
		if got := CanonicalISP(c.raw, false); got != c.want {
			t.Errorf("CanonicalISP(%q, false) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestCanonicalISPKeepsUnknownChineseISPAsIs(t *testing.T) {
	// 地方运营商本身就是有效信息，映射表不可能穷举，认不出的原样保留。
	for _, raw := range []string{"鹏博士宽带", "华数宽带", "长城宽带"} {
		if got := CanonicalISP(raw, false); got != raw {
			t.Errorf("CanonicalISP(%q, false) = %q, want 原样保留", raw, got)
		}
	}
}

func TestCanonicalISPForeignKeepsOnlyKnownIDC(t *testing.T) {
	// 境外只保留云厂商：全量保留会让纯真库段数涨到 3.3 倍。
	// 上游常带公司全称，所以必须能按关键词命中，不能只做精确匹配。
	for _, c := range []struct{ raw, want string }{
		{"Google LLC", "Google"},
		{"Amazon Technologies Inc.", "Amazon"},
		{"Cloudflare, Inc.", "Cloudflare"},
		{"Microsoft Corporation", "Microsoft"},
		{"Philippine Long Distance Telephone Company", ""},
		{"中华电信", ""},
		{"", ""},
	} {
		if got := CanonicalISP(c.raw, true); got != c.want {
			t.Errorf("CanonicalISP(%q, true) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestCanonicalISPKeepsIDCInsideChina(t *testing.T) {
	// 境内的云厂商同样要认出来：把账号挂在国内云主机上中转，
	// 与家宽来源的含义完全不同。
	for _, c := range []struct{ raw, want string }{
		{"腾讯云", "腾讯云"},
		{"腾讯", "腾讯云"},
		{"阿里", "阿里云"},
		{"阿里云", "阿里云"},
		{"华为云", "华为云"},
	} {
		if got := CanonicalISP(c.raw, false); got != c.want {
			t.Errorf("CanonicalISP(%q, false) = %q, want %q", c.raw, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./util/ipdb/ -run TestCanonicalISP -v`
Expected: 编译失败，`undefined: CanonicalISP`

- [ ] **Step 3: 实现归一化**

创建 `util/ipdb/isp_name.go`：

```go
package ipdb

import "strings"

// 本文件把两个数据源的运营商写法归一成一套统一名称，与 province_name.go
// 对省份做的事同构。
//
// 归一必须发生在落库前，不能挪到展示层，有两个各自独立的理由：
//
//   1. 纯真库的 ISP 字段有 115,619 种取值（"电信/新世纪网吧" 这类按接入点
//      拆出来的细分），原样入库会让归属地种类远超格式的 65536 上限，
//      BuildRecords 直接报错，整次更新失败并保留旧库——功能永远上不了线。
//   2. Multi.Lookup 的分歧判定是按字符串相等做的。ip2region 写「中国电信」、
//      纯真库写「电信」，不归一的话「存疑」标签会对几乎每个 IP 亮起，
//      把这个本来有用的提示废掉。

// ispAliases 是中国运营商的写法映射。key 一律小写，查表前先 ToLower。
var ispAliases = map[string]string{
	"电信": "中国电信", "中国电信": "中国电信", "chinanet": "中国电信",
	"china telecom": "中国电信", "中國電信": "中国电信",

	"联通": "中国联通", "中国联通": "中国联通", "网通": "中国联通",
	"中国网通": "中国联通", "china unicom": "中国联通", "unicom": "中国联通",

	"移动": "中国移动", "中国移动": "中国移动", "铁通": "中国移动",
	"中移铁通": "中国移动", "中国铁通": "中国移动", "china mobile": "中国移动",

	"广电网": "中国广电", "中国广电": "中国广电", "广电": "中国广电",

	"教育网": "教育网", "中国教育网": "教育网", "cernet": "教育网",
	"中国科技网": "科技网", "cstnet": "科技网",
}

// idcKeywords 是知名 IDC / 云厂商的关键词。上游常给公司全称
// （"Google LLC"、"Amazon Technologies Inc."），精确匹配命中不了，
// 必须按子串判定。
//
// 用切片而不是 map：一个字符串可能命中多个关键词，遍历顺序决定结果，
// 而 map 的遍历顺序是随机的——那会让同一份源数据每次生成出不同的库，
// 违反「生成逐字节确定」。
var idcKeywords = []struct{ keyword, name string }{
	{"cloudflare", "Cloudflare"},
	{"amazon", "Amazon"}, {"aws", "Amazon"}, {"亚马逊", "Amazon"},
	{"google", "Google"}, {"谷歌", "Google"},
	{"microsoft", "Microsoft"}, {"azure", "Microsoft"}, {"微软", "Microsoft"},
	{"akamai", "Akamai"}, {"阿卡迈", "Akamai"},
	{"fastly", "Fastly"},
	{"oracle", "Oracle"}, {"甲骨文", "Oracle"},
	{"digitalocean", "DigitalOcean"},
	{"linode", "Linode"},
	{"vultr", "Vultr"}, {"choopa", "Vultr"},
	{"hetzner", "Hetzner"},
	{"contabo", "Contabo"},
	{"leaseweb", "LeaseWeb"},
	{"scaleway", "Scaleway"},
	{"ovh", "OVH"},
	{"腾讯", "腾讯云"}, {"tencent", "腾讯云"},
	{"阿里", "阿里云"}, {"alibaba", "阿里云"}, {"aliyun", "阿里云"},
	{"华为", "华为云"}, {"huawei", "华为云"},
	{"百度", "百度云"}, {"baidu", "百度云"},
	{"ucloud", "UCloud"},
	{"金山", "金山云"}, {"kingsoft", "金山云"},
}

// cleanISP 做落库前的清洗：去空白、去掉两个源各自的「未知」占位，
// 并截掉纯真库那种 "电信/新世纪网吧" 的接入点后缀。
func cleanISP(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" {
		return ""
	}
	// CZ88.NET 是纯真库的广告占位，表示「这一段没有运营商信息」。
	if strings.Contains(strings.ToUpper(s), "CZ88") {
		return ""
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

func matchIDC(lower string) string {
	for _, e := range idcKeywords {
		if strings.Contains(lower, e.keyword) {
			return e.name
		}
	}
	return ""
}

// CanonicalISP 把任意写法的运营商名归一成本项目的统一名称。
//
// foreign 为 true（这条记录的国家不是中国）时只保留知名 IDC / 云厂商，
// 其余一律返回空串：境外段的 ISP 字段极其细碎，全量保留会让相邻段的合并
// 几乎完全失效，纯真库的段数从 261,670 涨到 858,363，文件与常驻内存跟着涨
// 3.3 倍。而境外来源 IP 真正有价值的信息就是「它是不是机房」。
func CanonicalISP(raw string, foreign bool) string {
	s := cleanISP(raw)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if idc := matchIDC(lower); idc != "" {
		return idc
	}
	if foreign {
		return ""
	}
	if isp, ok := ispAliases[lower]; ok {
		return isp
	}
	// 认不出的中国运营商原样保留：「鹏博士宽带」这类地方运营商本身就是
	// 有效信息，映射表不可能穷举。
	return s
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./util/ipdb/ -run TestCanonicalISP -v`
Expected: 5 个测试全部 PASS

- [ ] **Step 5: 提交**

```bash
git add util/ipdb/isp_name.go util/ipdb/isp_name_test.go
git commit -m "feat(ipdb): 新增运营商名称归一化"
```

---

### Task 2: ipdb 格式升级到 v2，`Parse` 兼容 v1

**Files:**
- Modify: `util/ipdb/ipdb.go`（`Record`、`Location`、常量、`BuildRecords`、新增 `HasISP`）
- Modify: `util/ipdb/parse.go`（`Parse` 按版本分支）
- Test: `util/ipdb/ipdb_test.go`（新增用例）

**Interfaces:**
- Consumes: `CanonicalISP`（Task 1，本任务不调用，Task 3/4 才调）
- Produces:
  - `ipdb.Location` 多一个字段 `ISP string`
  - `ipdb.Record` 多一个字段 `ISP string`
  - `func (d *DB) HasISP() bool` — Task 5 用它决定要不要带条件请求头

- [ ] **Step 1: 写失败的测试**

在 `util/ipdb/ipdb_test.go` 末尾追加：

```go
// v2 写入的 ISP 必须能原样读回来。
func TestBuildRecordsRoundTripsISP(t *testing.T) {
	var buf bytes.Buffer
	recs := []Record{
		{Start: 0x01000000, End: 0x0100FFFF, Country: "中国", Region: "江苏省", City: "南京市", ISP: "中国电信"},
		{Start: 0x01010000, End: 0x0101FFFF, Country: "中国", Region: "江苏省", City: "南京市", ISP: "中国联通"},
		{Start: 0x03000000, End: 0x03FFFFFF, Country: "United States", ISP: "Google"},
	}
	if err := BuildRecords(recs, &buf, testBuiltAt); err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	db, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !db.HasISP() {
		t.Error("HasISP() = false, 新生成的库应当是 v2")
	}
	for _, c := range []struct{ ip, isp string }{
		{"1.0.0.1", "中国电信"},
		{"1.1.0.1", "中国联通"},
		{"3.1.1.1", "Google"},
	} {
		loc, ok := db.Lookup(net.ParseIP(c.ip))
		if !ok {
			t.Errorf("Lookup(%s) 未命中", c.ip)
			continue
		}
		if loc.ISP != c.isp {
			t.Errorf("Lookup(%s).ISP = %q, want %q", c.ip, loc.ISP, c.isp)
		}
	}
}

// 同城不同 ISP 的相邻段不能再被合并掉，否则 ISP 信息会丢。
func TestBuildRecordsDoesNotMergeAdjacentSegmentsWithDifferentISP(t *testing.T) {
	var buf bytes.Buffer
	recs := []Record{
		{Start: 0x02000000, End: 0x020000FF, Country: "中国", Region: "江苏省", City: "南京市", ISP: "中国电信"},
		{Start: 0x02000100, End: 0x020001FF, Country: "中国", Region: "江苏省", City: "南京市", ISP: "中国联通"},
	}
	if err := BuildRecords(recs, &buf, testBuiltAt); err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	db, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if db.SegmentCount() != 2 {
		t.Errorf("SegmentCount = %d, want 2（同城不同 ISP 不能合并）", db.SegmentCount())
	}
}

// 升级路径的关键用例：机器上已有的 v1 库必须还能读，ISP 留空。
// 拒绝加载会让整个归属地列当场消失——不只是新的运营商列，
// 连现在能用的省市判定和共享检测的省份判定也一起失效。
func TestParseAcceptsLegacyV1File(t *testing.T) {
	data := buildLegacyV1(t)
	db, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse v1: %v", err)
	}
	if db.HasISP() {
		t.Error("HasISP() = true, v1 库不含 ISP")
	}
	loc, ok := db.Lookup(net.ParseIP("1.0.0.1"))
	if !ok {
		t.Fatal("Lookup 未命中")
	}
	if loc.Country != "中国" || loc.Region != "江苏省" || loc.City != "南京市" {
		t.Errorf("got %+v, want 中国/江苏省/南京市", loc)
	}
	if loc.ISP != "" {
		t.Errorf("ISP = %q, v1 库读出来必须是空串", loc.ISP)
	}
}

// buildLegacyV1 手工拼一份 v1 格式的库：1 个段、1 个归属地。
// 直接写字节而不是留一个 v1 的构建函数在生产代码里——v1 只需要能读，
// 不需要能写。
//
// v1 线格式（全部小端）：
//
//	头部 32 字节：magic(8) + version(4) + builtAt(8) + segCount(4) + locCount(2) + 填充
//	段 10 字节：start(4) + end(4) + locIndex(2)
//	归属地 6 字节：country(2) + region(2) + city(2)，都是字符串池下标
//	字符串池：count(4) + 每项 len(2) + 字节
func buildLegacyV1(t *testing.T) []byte {
	t.Helper()
	pool := []string{"", "中国", "江苏省", "南京市"}

	buf := make([]byte, headerSize)
	copy(buf, magic)
	binary.LittleEndian.PutUint32(buf[8:], 1)
	binary.LittleEndian.PutUint64(buf[12:], uint64(testBuiltAt.Unix()))
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[24:], 1)

	seg := make([]byte, 10)
	binary.LittleEndian.PutUint32(seg[0:], 0x01000000)
	binary.LittleEndian.PutUint32(seg[4:], 0x0100FFFF)
	binary.LittleEndian.PutUint16(seg[8:], 0)
	buf = append(buf, seg...)

	loc := make([]byte, 6)
	binary.LittleEndian.PutUint16(loc[0:], 1)
	binary.LittleEndian.PutUint16(loc[2:], 2)
	binary.LittleEndian.PutUint16(loc[4:], 3)
	buf = append(buf, loc...)

	var n4 [4]byte
	binary.LittleEndian.PutUint32(n4[:], uint32(len(pool)))
	buf = append(buf, n4[:]...)
	for _, s := range pool {
		var n2 [2]byte
		binary.LittleEndian.PutUint16(n2[:], uint16(len(s)))
		buf = append(buf, n2[:]...)
		buf = append(buf, s...)
	}
	return buf
}
```

在 `util/ipdb/ipdb_test.go` 的 import 块加上 `"encoding/binary"`。

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./util/ipdb/ -run 'TestBuildRecordsRoundTripsISP|TestParseAcceptsLegacyV1File|TestBuildRecordsDoesNotMerge' -v`
Expected: 编译失败，`unknown field ISP in struct literal` 与 `db.HasISP undefined`

- [ ] **Step 3: 改 `util/ipdb/ipdb.go`**

常量与结构体：

```go
const (
	magic         = "AUIPDB01"
	formatVersion = uint32(2)
	// legacyFormatVersion 是不含 ISP 的旧格式。Parse 必须继续认它：
	// 面板升级后机器上的库还是这个版本，拒绝加载会让整个归属地列当场
	// 消失（连带共享检测的省份判定），而界面上只显示「未加载」，
	// 看不出是格式版本的问题。
	legacyFormatVersion = uint32(1)

	headerSize = 32
	segmentSize = 10
	// locationSize 是 v2 的归属地表项大小（四个字符串池下标）。
	locationSize = 8
	// legacyLocationSize 是 v1 的（三个下标，没有 ISP）。
	legacyLocationSize = 6

	chinaCountry = "中国"
)
```

`Record` 与 `Location` 各加一个字段（注释说明境外只在命中云厂商时才有值）：

```go
type Record struct {
	Start, End uint32
	Country    string
	Region     string
	City       string
	// ISP 已经过 CanonicalISP 归一。境外段只在命中知名 IDC / 云厂商时非空。
	ISP string
}

type Location struct {
	Country string
	Region  string
	City    string
	ISP     string
}
```

`DB` 加一个字段与访问器：

```go
type DB struct {
	builtAt   time.Time
	segments  []segment
	locations []Location
	// hasISP 记录这份库是不是 v2。v1 库能被正常读出来，但所有 ISP 都是空的，
	// 调用方需要区分「这个 IP 没有运营商信息」与「这份库根本不带运营商」。
	hasISP bool
}

// HasISP 报告这份库是否是含运营商信息的新格式。
func (d *DB) HasISP() bool { return d.hasISP }
```

本任务**不动 `normalize`**：改它的签名就必须同时改 `parseIP2Region` 的调用点，
那属于 Task 3。本任务结束时 `parseIP2Region` 仍然产出 ISP 为空的 Record，
格式已经能存能读，测试用 `BuildRecords` 直接喂带 ISP 的 Record 来验证。

`BuildRecords` 里 loc 的构造与写入：

```go
		loc := Location{Country: r.Country, Region: r.Region, City: r.City, ISP: r.ISP}
```

```go
			intern(loc.Country)
			intern(loc.Region)
			intern(loc.City)
			intern(loc.ISP)
```

```go
	buf = buf[:locationSize]
	for _, loc := range locations {
		binary.LittleEndian.PutUint16(buf[0:], uint16(strIndex[loc.Country]))
		binary.LittleEndian.PutUint16(buf[2:], uint16(strIndex[loc.Region]))
		binary.LittleEndian.PutUint16(buf[4:], uint16(strIndex[loc.City]))
		binary.LittleEndian.PutUint16(buf[6:], uint16(strIndex[loc.ISP]))
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
```

- [ ] **Step 4: 改 `util/ipdb/parse.go` 按版本分支**

把版本校验与归属地表读取改成：

```go
	version := binary.LittleEndian.Uint32(data[8:])
	var locSize int
	switch version {
	case formatVersion:
		locSize = locationSize
	case legacyFormatVersion:
		locSize = legacyLocationSize
	default:
		return nil, common.NewErrorf("格式版本为 %d，本程序只支持 %d 与 %d",
			version, legacyFormatVersion, formatVersion)
	}
```

归属地表读取（把 `[3]uint16` 换成 `[4]uint16`，v1 的第 4 项恒为 0，也就是字符串池里的空串）：

```go
	if need := off + locCount*locSize; len(data) < need {
		return nil, common.NewErrorf("归属地表被截断: 需要 %d 字节，实际 %d", need, len(data))
	}
	rawLocs := make([][4]uint16, locCount)
	for i := 0; i < locCount; i++ {
		b := data[off+i*locSize:]
		rawLocs[i] = [4]uint16{
			binary.LittleEndian.Uint16(b[0:]),
			binary.LittleEndian.Uint16(b[2:]),
			binary.LittleEndian.Uint16(b[4:]),
		}
		if locSize == locationSize {
			rawLocs[i][3] = binary.LittleEndian.Uint16(b[6:])
		}
	}
	off += locCount * locSize
```

组装 Location：

```go
		locations[i] = Location{
			Country: pool[raw[0]],
			Region:  pool[raw[1]],
			City:    pool[raw[2]],
			ISP:     pool[raw[3]],
		}
```

返回值带上版本标记：

```go
	return &DB{
		builtAt:   builtAt,
		segments:  segments,
		locations: locations,
		hasISP:    version == formatVersion,
	}, nil
```

- [ ] **Step 5: 运行整个包的测试**

Run: `go test ./util/ipdb/ -v`
Expected: 全部 PASS。注意 `realdata_test.go` 对仓库里那份 v1 的 `bin/ipdb.dat` 的断言也必须继续通过——那正是 v1 兼容读的真实验证。

- [ ] **Step 6: 提交**

```bash
git add util/ipdb/ipdb.go util/ipdb/parse.go util/ipdb/ipdb_test.go
git commit -m "feat(ipdb): 紧凑格式升到 v2 并带上运营商，Parse 继续兼容 v1"
```

---

### Task 3: ip2region 解析放开 ISP + `CIDRsOfProvinces` 不变性回归

**Files:**
- Modify: `util/ipdb/ipdb.go`（`parseIP2Region`）
- Test: `util/ipdb/ipdb_test.go`

**Interfaces:**
- Consumes: `CanonicalISP`（Task 1）、`Record.ISP`（Task 2）
- Produces: 无新导出符号

- [ ] **Step 1: 写失败的测试**

追加到 `util/ipdb/ipdb_test.go`：

```go
// sampleSource 的第 6 字段就是 ISP，此前被丢掉了。
func TestBuildReadsISPFromIP2RegionSource(t *testing.T) {
	db := buildSample(t, sampleSource)
	for _, c := range []struct{ ip, isp string }{
		{"1.0.2.5", "中国电信"},
		{"2.0.1.5", "中国联通"},
		{"3.1.2.3", ""}, // 源里是 "0"，境外且非云厂商
	} {
		loc, ok := db.Lookup(net.ParseIP(c.ip))
		if !ok {
			t.Errorf("Lookup(%s) 未命中", c.ip)
			continue
		}
		if loc.ISP != c.isp {
			t.Errorf("Lookup(%s).ISP = %q, want %q", c.ip, loc.ISP, c.isp)
		}
	}
}

// 加 ISP 之后段变细了，但 CIDRsOfProvinces 的输出必须逐字节不变：
// 它的结果进 geo dat 的内容哈希，一变就会让那个 10 秒的 cron 反复重启 xray。
//
// 之所以能不变：CIDRsOfProvinces 按 Region 过滤后自己重新合并连续区间
// （hasRange && end+1 == s.start），不依赖 segments 的合并粒度。
func TestCIDRsOfProvincesUnaffectedByISPSplit(t *testing.T) {
	// 同一批数据的两种口径：一份不带 ISP（相邻同城段会合并），
	// 一份带 ISP（同城不同 ISP 拆成两段）。
	base := []Record{
		{Start: 0x02000000, End: 0x020000FF, Country: "中国", Region: "江苏省", City: "南京市"},
		{Start: 0x02000100, End: 0x020001FF, Country: "中国", Region: "江苏省", City: "南京市"},
		{Start: 0x02000200, End: 0x020002FF, Country: "中国", Region: "江苏省", City: "苏州市"},
	}
	withISP := make([]Record, len(base))
	copy(withISP, base)
	withISP[0].ISP = "中国电信"
	withISP[1].ISP = "中国联通"
	withISP[2].ISP = "中国电信"

	build := func(recs []Record) *DB {
		t.Helper()
		var buf bytes.Buffer
		if err := BuildRecords(recs, &buf, testBuiltAt); err != nil {
			t.Fatalf("BuildRecords: %v", err)
		}
		db, err := Parse(buf.Bytes())
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return db
	}

	plain, split := build(base), build(withISP)
	if plain.SegmentCount() == split.SegmentCount() {
		t.Fatalf("两种口径的段数相同（都是 %d），用例没有覆盖到分裂场景",
			plain.SegmentCount())
	}
	want := plain.CIDRsOfProvinces([]string{"江苏省"})
	got := split.CIDRsOfProvinces([]string{"江苏省"})
	if len(want) != len(got) {
		t.Fatalf("CIDR 条数变了：不带 ISP %v，带 ISP %v", want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("第 %d 条 CIDR 变了: %q -> %q（geo dat 的内容哈希会跟着变，"+
				"那个 10 秒的 cron 会反复重启 xray）", i, want[i], got[i])
		}
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./util/ipdb/ -run 'TestBuildReadsISPFromIP2RegionSource|TestCIDRsOfProvincesUnaffected' -v`
Expected: `TestBuildReadsISPFromIP2RegionSource` FAIL（ISP 全是空串）；`TestCIDRsOfProvincesUnaffected` 也 FAIL（`两种口径的段数相同`，因为 ISP 还没进合并键）

- [ ] **Step 3: 改 `parseIP2Region` 读第 6 字段**

先给 `normalize` 加 ISP 参数。境外与境内的策略不同，由 `CanonicalISP` 的第二个
参数区分：

```go
func normalize(country, region, city, isp string) Location {
	if country == "Reserved" || country == "0" {
		country = ""
	}
	if country != chinaCountry {
		// 境外只保留国家与知名 IDC / 云厂商：全量保留 ISP 会让相邻段的合并
		// 几乎完全失效，纯真库的段数会涨到 3.3 倍。
		return Location{Country: country, ISP: CanonicalISP(isp, true)}
	}
	if region == "0" {
		region = ""
	}
	if city == "0" {
		city = ""
	}
	return Location{Country: country, Region: region, City: city, ISP: CanonicalISP(isp, false)}
}
```

再改 `parseIP2Region` 的调用点，把第 6 字段传进去：

```go
		loc := normalize(fields[2], fields[3], fields[4], fields[5])
		records = append(records, Record{
			Start: start, End: end,
			Country: loc.Country, Region: loc.Region, City: loc.City, ISP: loc.ISP,
		})
```

同时更新包头注释里那句已经过时的精度说明：

```go
// 精度取舍：境外只保留国家与知名 IDC / 云厂商，中国保留到省+市+运营商。
```

- [ ] **Step 4: 运行整包测试**

Run: `go test ./util/ipdb/ -v`
Expected: 全部 PASS

- [ ] **Step 5: 提交**

```bash
git add util/ipdb/ipdb.go util/ipdb/ipdb_test.go
git commit -m "feat(ipdb): ip2region 解析读出运营商字段"
```

---

### Task 4: 纯真库解析放开 ISP

**Files:**
- Modify: `util/qqwry/qqwry.go`（`toRecord`）
- Test: `util/qqwry/qqwry_test.go`

**Interfaces:**
- Consumes: `ipdb.CanonicalISP`（Task 1）、`ipdb.Record.ISP`（Task 2）
- Produces: 无新导出符号

- [ ] **Step 1: 写失败的测试**

追加到 `util/qqwry/qqwry_test.go`。`toRecord` 是包内函数、直接调用，不需要拼字节，
文件里那个 `builder` 用不上：

```go
// 纯真库记录的第二段文本就是运营商，此前被 `_ = area` 丢掉了。
// 归一必须在这里做，不能留到展示层：这个字段有 115,619 种取值，
// 原样入库会撞上格式的 65536 种上限、整次更新失败。
func TestToRecordKeepsNormalizedISP(t *testing.T) {
	for _, c := range []struct {
		country, area string
		wantISP       string
	}{
		{"中国–江苏–南京", "电信", "中国电信"},
		{"中国–江苏–南京", "电信/新世纪网吧", "中国电信"},
		{"中国–山东–聊城", "联通", "中国联通"},
		{"中国–北京–北京", " CZ88.NET", ""},
		{"美国–加利福尼亚", "Google LLC", "Google"},
		{"美国–加利福尼亚", "Comcast Cable", ""},
	} {
		rec := toRecord(0x01000000, 0x0100FFFF, c.country, c.area)
		if rec.ISP != c.wantISP {
			t.Errorf("toRecord(%q, %q).ISP = %q, want %q",
				c.country, c.area, rec.ISP, c.wantISP)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./util/qqwry/ -run TestToRecordKeepsNormalizedISP -v`
Expected: FAIL，所有 ISP 都是空串

- [ ] **Step 3: 改 `toRecord`**

`util/qqwry/qqwry.go`，把 `_ = area` 那行删掉，改成真的用它。注意 ISP 的归一要传对 `foreign`——境外与境内的策略不同，而这里的 `rec.Country` 在两个分支里都已经确定：

```go
func toRecord(start, end uint32, country, area string) ipdb.Record {
	parts := strings.Split(strings.TrimSpace(country), areaSeparator)
	rec := ipdb.Record{Start: start, End: end, Country: strings.TrimSpace(parts[0])}
	// 第二段文本就是运营商。归一必须在落库前做：这个字段在真实库里有
	// 十万级的取值（"电信/新世纪网吧" 这类按接入点拆的细分），原样入库
	// 会让归属地种类撞上格式的 65536 上限，整次更新失败。
	rec.ISP = ipdb.CanonicalISP(area, rec.Country != chinaCountry)
	if rec.Country != chinaCountry {
		// 境外只保留国家（与 ip2region 那一路一致）与已归一的云厂商标识。
		return rec
	}
	if len(parts) < 2 {
		return rec
	}
	province, ok := ipdb.CanonicalProvince(parts[1])
	if !ok {
		return rec
	}
	rec.Region = province
	if len(parts) >= 3 {
		rec.City = strings.TrimSpace(parts[2])
	}
	return rec
}
```

同时更新包头注释里「ISP 信息本包不保留」的说法（那句已不成立）。

- [ ] **Step 4: 运行整包测试**

Run: `go test ./util/qqwry/ -v`
Expected: 全部 PASS

- [ ] **Step 5: 提交**

```bash
git add util/qqwry/qqwry.go util/qqwry/qqwry_test.go
git commit -m "feat(qqwry): 纯真库解析读出运营商字段"
```

---

### Task 5: 本地库还是 v1 时不带条件请求头

**Files:**
- Modify: `web/service/ipdb.go:352`（`fetchAndBuild` 的条件请求判据）
- Test: `web/service/ipdb_test.go`

**Interfaces:**
- Consumes: `(*ipdb.DB).HasISP()`（Task 2）
- Produces: 无新导出符号

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/ipdb_test.go`：

```go
// 升级后本地那份库还是 v1（不含运营商）。它能被兼容的 Parse 正常加载，
// 于是 dbOf(...) != nil 成立、带上 ETag、上游文件没变、服务端回 304、
// 库不重建——运营商列会永远是空的，管理员点「更新」还只会得到「已是最新」。
// 判据必须收紧成「本地那份确实在用**且已经是当前格式**」。
func TestFetchAndBuildSkipsConditionalRequestWhenLocalDatabaseIsLegacy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ipdb.dat")

	legacy, err := ipdb.Parse(legacyV1Bytes(t))
	if err != nil {
		t.Fatalf("解析 v1 样本: %v", err)
	}

	var gotConditional bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			gotConditional = true
		}
		_, _ = w.Write([]byte(ipdbSampleSource))
	}))
	t.Cleanup(srv.Close)

	src := testSource(path, 1)
	src.EtagKey = "testEtag"

	s := IPDBService{}
	s.setDB(src.Key, legacy)
	t.Cleanup(func() { s.setDB(src.Key, nil) })

	if _, err := s.fetchAndBuild(src, srv.URL, path); err != nil {
		t.Fatalf("fetchAndBuild: %v", err)
	}
	if gotConditional {
		t.Error("本地库还是 v1 时不该带 If-None-Match：带了会被 304 挡回，库永远换不成 v2")
	}
}

// 反面：本地库已经是 v2 时，条件请求照旧，不能因为这次改动多下几十 MB。
func TestFetchAndBuildKeepsConditionalRequestWhenLocalDatabaseIsCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ipdb.dat")
	current := seedDatabaseAt(t, path, time.Now())

	var gotConditional bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			gotConditional = true
		}
		_, _ = w.Write([]byte(ipdbSampleSource))
	}))
	t.Cleanup(srv.Close)

	src := testSource(path, 1)
	src.EtagKey = "testEtag"

	s := IPDBService{}
	s.setDB(src.Key, current)
	t.Cleanup(func() { s.setDB(src.Key, nil) })

	// 先记一个 ETag，否则判据后面那半段（etag != ""）不成立。
	s.rememberEtag(src, `"deadbeef"`)

	if _, err := s.fetchAndBuild(src, srv.URL, path); err != nil {
		t.Fatalf("fetchAndBuild: %v", err)
	}
	if !gotConditional {
		t.Error("本地库已经是 v2 时应当继续带 If-None-Match")
	}
}

// legacyV1Bytes 手工拼一份 v1 库，与 util/ipdb 的同名 helper 是同一套线格式。
// 在这个包里重拼一份而不是从 util/ipdb 导出：v1 只需要在测试里造得出来，
// 不该为此在生产代码上开一个导出函数。
func legacyV1Bytes(t *testing.T) []byte {
	t.Helper()
	pool := []string{"", "中国", "江苏省", "南京市"}

	buf := make([]byte, 32)
	copy(buf, "AUIPDB01")
	binary.LittleEndian.PutUint32(buf[8:], 1)
	binary.LittleEndian.PutUint64(buf[12:], uint64(time.Now().Unix()))
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[24:], 1)

	seg := make([]byte, 10)
	binary.LittleEndian.PutUint32(seg[0:], 0x01000000)
	binary.LittleEndian.PutUint32(seg[4:], 0x0100FFFF)
	buf = append(buf, seg...)

	loc := make([]byte, 6)
	binary.LittleEndian.PutUint16(loc[0:], 1)
	binary.LittleEndian.PutUint16(loc[2:], 2)
	binary.LittleEndian.PutUint16(loc[4:], 3)
	buf = append(buf, loc...)

	var n4 [4]byte
	binary.LittleEndian.PutUint32(n4[:], uint32(len(pool)))
	buf = append(buf, n4[:]...)
	for _, s := range pool {
		var n2 [2]byte
		binary.LittleEndian.PutUint16(n2[:], uint16(len(s)))
		buf = append(buf, n2[:]...)
		buf = append(buf, s...)
	}
	return buf
}
```

在 `web/service/ipdb_test.go` 的 import 块补上 `"encoding/binary"`（`net/http`、`net/http/httptest`、`path/filepath`、`time` 已有）。

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run 'TestFetchAndBuildSkipsConditionalRequestWhenLocalDatabaseIsLegacy' -v`
Expected: FAIL，`本地库还是 v1 时不该带 If-None-Match`

- [ ] **Step 3: 改判据**

`web/service/ipdb.go` 的 `fetchAndBuild` 里：

```go
	// 只有本地那份确实在用、且已经是当前格式时才带条件头。
	//
	// 「在用」那半段管的是首次安装与文件被删：库不在时收到 304 就什么都
	// 拿不到，而库缺失恰恰是最需要下载的时候。
	//
	// 「已经是当前格式」那半段管的是升级：旧格式的库能被兼容的 Parse 正常
	// 加载，带上 ETag 就会被 304 挡回，库永远升不上来，新字段永远是空的，
	// 而管理员点「更新」只会得到「已是最新」——一个完全静默的失效。
	// 判据写成「格式对不对」而不是「升级过没有」，是为了自愈：任何原因让
	// 库退回旧格式（回退过一次二进制、手工拷了旧文件），下次更新都会重建。
	if cur := s.dbOf(src.Key); src.EtagKey != "" && cur != nil && cur.HasISP() {
		etag, err := s.settingService.getOptionalString(src.EtagKey)
		if err != nil {
			logger.Warning("读取 IP 库 ETag 失败, 源:", src.Name, "err:", err)
		} else if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
	}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run 'TestFetchAndBuild' -v`
Expected: 新增两条与既有的 `TestFetchAndBuild*` 全部 PASS

- [ ] **Step 5: 提交**

```bash
git add web/service/ipdb.go web/service/ipdb_test.go
git commit -m "fix(ipdb): 本地库还是旧格式时跳过条件请求，避免被 304 挡住升级"
```

---

### Task 6: 服务层把运营商带到三个接口

**Files:**
- Modify: `web/service/online.go`（`OnlineIP`、`locate`、`locateWithIPDB`、两个组装点）
- Modify: `web/service/accesslog.go`（`AccessLogRow`、`RecentSource`、两个组装点）
- Test: `web/service/online_test.go`

**Interfaces:**
- Consumes: `ipdb.Location.ISP`（Task 2）
- Produces:
  - `type ipLocation struct { Location, LocationAlt, ISP, ISPAlt string }`
  - `func locateWithIPDB(svc IPDBService, ip net.IP) ipLocation`
  - JSON 字段：`OnlineIP.isp` / `OnlineIP.ispAlt`、`AccessLogRow.isp` / `.ispAlt`、`RecentSource.isp` / `.ispAlt`（Task 7 的前端按这些名字取值）

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/online_test.go`：

```go
// 运营商与归属地走同一次判定：两个源不一致时把分歧带出来，不做仲裁。
func TestLocateWithIPDBReportsISPDisagreement(t *testing.T) {
	// 构造两个源：同一个 IP，归属地相同、运营商不同。
	primary := buildISPTestDB(t, "中国电信")
	alt := buildISPTestDB(t, "中国联通")
	svc := ipdbServiceWithSources(t, primary, alt)

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))
	if got.ISP != "中国电信" {
		t.Errorf("ISP = %q, want 中国电信", got.ISP)
	}
	if got.ISPAlt != "中国联通" {
		t.Errorf("ISPAlt = %q, want 中国联通（分歧必须显示出来，不做仲裁）", got.ISPAlt)
	}
}

// 两个源一致时不该报分歧，否则「存疑」标签会对几乎每个 IP 亮起。
func TestLocateWithIPDBReportsNoDisagreementWhenSourcesAgree(t *testing.T) {
	db := buildISPTestDB(t, "中国电信")
	svc := ipdbServiceWithSources(t, db, buildISPTestDB(t, "中国电信"))

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))
	if got.ISPAlt != "" {
		t.Errorf("ISPAlt = %q, 两个源一致时必须为空", got.ISPAlt)
	}
}
```

两个 helper 也写在 `online_test.go` 里：

```go
// buildISPTestDB 造一份只含一段的库：1.0.0.0-1.0.255.255，中国/江苏省/南京市，
// 运营商由调用方指定。
func buildISPTestDB(t *testing.T, isp string) *ipdb.DB {
	t.Helper()
	var buf bytes.Buffer
	recs := []ipdb.Record{{
		Start: 0x01000000, End: 0x0100FFFF,
		Country: "中国", Region: "江苏省", City: "南京市", ISP: isp,
	}}
	if err := ipdb.BuildRecords(recs, &buf, time.Unix(1788000000, 0)); err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	db, err := ipdb.Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return db
}

// ipdbServiceWithSources 把全局的数据源列表换成两个测试源，并塞入给定的两份库。
// 顺序即 Multi.Lookup 的返回顺序，也就决定了谁是主判定、谁是 alt。
func ipdbServiceWithSources(t *testing.T, primary, alt *ipdb.DB) IPDBService {
	t.Helper()
	dir := t.TempDir()
	sources := []ipdbSource{
		testSource(filepath.Join(dir, "primary.dat"), 1),
		testSource(filepath.Join(dir, "alt.dat"), 1),
	}
	sources[0].Key = "primary"
	sources[1].Key = "alt"
	useTestSources(t, sources)

	s := IPDBService{}
	s.setDB("primary", primary)
	s.setDB("alt", alt)
	t.Cleanup(func() {
		s.setDB("primary", nil)
		s.setDB("alt", nil)
	})
	return s
}
```

`online_test.go` 的 import 需要 `bytes`、`net`、`path/filepath`、`time` 与 `a-ui/util/ipdb`；已有的按现状保留，不重复添加。

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run TestLocateWithIPDB -v`
Expected: 编译失败，`got.ISP undefined`（`locateWithIPDB` 还返回两个 string）

- [ ] **Step 3: 改 `web/service/online.go`**

`OnlineIP` 加两个字段（紧跟 `LocationAlt`）：

```go
	// ISP 是来源 IP 的运营商，已归一（"中国电信"/"腾讯云"/"Google"）。
	// 境外 IP 只在命中知名 IDC / 云厂商时非空——那正是「这个来源是机房」
	// 这个判断所需的全部信息。
	ISP string `json:"isp"`
	// ISPAlt 与 LocationAlt 同理：另一个数据源给出的、与主判定不同的运营商。
	ISPAlt string `json:"ispAlt"`
```

`locateWithIPDB` 换成返回结构体。四个返回值里两两同类型，用位置传参极易写反：

```go
// ipLocation 是一次归属地判定的完整结果。
//
// 不用四个 string 返回值：Location/LocationAlt/ISP/ISPAlt 类型相同，
// 调用点写反了不会有任何编译错误，只会让界面上的主判定与分歧对调。
type ipLocation struct {
	Location    string
	LocationAlt string
	ISP         string
	ISPAlt      string
}

// locateWithIPDB 判定 IP 的归属地与运营商，并把两个源的分歧一并带出来。
// 在线明细与访问日志的来源列表共用同一套判定，避免三处结论对不上。
//
// 不做仲裁：实测两个离线库对同一批 IP 互有出入，谁也不是权威。把分歧原样
// 显示给管理员，比替他挑一个更有用。归属地与运营商各自独立判定分歧——
// 两个源完全可能在城市名上有出入而运营商一致。
func locateWithIPDB(svc IPDBService, ip net.IP) ipLocation {
	var out ipLocation
	db := svc.DB()
	if db == nil {
		return out
	}
	for _, sl := range db.Lookup(ip) {
		if text := formatLocation(sl.Location); text != "" {
			if out.Location == "" {
				out.Location = text
			} else if text != out.Location && out.LocationAlt == "" {
				out.LocationAlt = text
			}
		}
		if isp := sl.Location.ISP; isp != "" {
			if out.ISP == "" {
				out.ISP = isp
			} else if isp != out.ISP && out.ISPAlt == "" {
				out.ISPAlt = isp
			}
		}
	}
	return out
}
```

`locate` 方法与两个组装点跟着改：

```go
func (s *OnlineService) locate(ip net.IP) ipLocation {
	return locateWithIPDB(s.ipdbService, ip)
}
```

`online.go:278` 与 `:303` 附近，把原来的 `primary, alt` 两个变量换成一个 `loc := s.locate(...)`，组装时写：

```go
			Location:    loc.Location,
			LocationAlt: loc.LocationAlt,
			ISP:         loc.ISP,
			ISPAlt:      loc.ISPAlt,
```

- [ ] **Step 4: 改 `web/service/accesslog.go` 的两个结构体与两个组装点**

`AccessLogRow` 与 `RecentSource` 各加：

```go
	ISP    string `json:"isp"`
	ISPAlt string `json:"ispAlt"`
```

`accesslog.go:219` 附近的记忆化缓存把 `type located struct{ primary, alt string }` 换成直接缓存 `ipLocation`：

```go
	seen := make(map[string]ipLocation, len(list))
	rows := make([]AccessLogRow, 0, len(list))
	for _, r := range list {
		loc, ok := seen[r.SourceIP]
		if !ok {
			loc = locateWithIPDB(s.ipdbService, net.ParseIP(r.SourceIP))
			seen[r.SourceIP] = loc
		}
		rows = append(rows, AccessLogRow{
			AccessLog:   r,
			Location:    loc.Location,
			LocationAlt: loc.LocationAlt,
			ISP:         loc.ISP,
			ISPAlt:      loc.ISPAlt,
		})
	}
```

`accesslog.go:317` 附近的 `RecentSource` 组装同理。

- [ ] **Step 5: 运行测试**

Run: `go test ./web/... -v -run 'TestLocateWithIPDB|TestOnline|TestAccessLog'`
Expected: 全部 PASS

- [ ] **Step 6: 提交**

```bash
git add web/service/online.go web/service/accesslog.go web/service/online_test.go
git commit -m "feat(online): 在线明细与访问日志返回来源 IP 的运营商"
```

---

### Task 7: 前端两处显示

**Files:**
- Modify: `web/html/xui/inbounds.html`（展开行列定义 + 一个 slot 模板）
- Modify: `web/html/xui/access_log_modal.html`（列定义 + location slot + 近期来源 tooltip）
- Test: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot'`

**Interfaces:**
- Consumes: `online.isp` / `online.ispAlt`、`row.isp` / `row.ispAlt`、`src.isp` / `src.ispAlt`（Task 6）
- Produces: 无

- [ ] **Step 1: 给 `inbounds.html` 加列定义**

在 `onlineColumns` 的「归属地」之后插入：

```javascript
    }, {
        title: "运营商",
        align: 'center',
        width: 110,
        scopedSlots: { customRender: 'isp' },
```

- [ ] **Step 2: 加 slot 模板**

紧跟现有的 `location` slot 之后：

```html
                                    <template slot="isp" slot-scope="text, online">
                                        <a-tag v-if="online.isp" :color="ispColor(online.isp)">[[ online.isp ]]</a-tag>
                                        <a-tag v-else>未知</a-tag>
                                    </template>
```

- [ ] **Step 3: 加 `ispColor` 方法**

在 `inbounds.html` 的 Vue 实例 `methods` 里（与 `banDesc` 同级）：

```javascript
        // 机房 / 云厂商来源单独用一个颜色：家宽来源与「把账号挂在服务器上
        // 中转」是完全不同的两件事，颜色让它在扫读时能立刻区分出来。
        // 判据是「不在三大运营商这几个名字里」——CanonicalISP 已经把认得出的
        // 运营商归一成了固定的几个字符串，剩下的要么是云厂商，要么是地方
        // 运营商，后者按机房色显示只是偏保守，不会误导。
        ispColor(isp) {
            return ['中国电信', '中国联通', '中国移动', '中国广电', '教育网', '科技网'].includes(isp)
                ? 'blue' : 'purple';
        },
```

- [ ] **Step 4: 把运营商的分歧并进现有的「存疑」标签**

`inbounds.html` 里现有的 `location` slot 中那个 `a-tooltip v-if="online.locationAlt"` 改成同时看两个字段：

```html
                                        <a-tooltip v-if="online.locationAlt || online.ispAlt">
                                            <template slot="title">
                                                两个离线库判定不一致。
                                                <template v-if="online.locationAlt">
                                                    <br>归属地：另一个源认为是 [[ online.locationAlt ]]
                                                </template>
                                                <template v-if="online.ispAlt">
                                                    <br>运营商：另一个源认为是 [[ online.ispAlt ]]
                                                </template>
                                            </template>
                                            <a-tag color="orange">存疑</a-tag>
                                        </a-tooltip>
```

- [ ] **Step 5: 改 `access_log_modal.html`**

列定义里「归属地」之后插一列：

```javascript
        }, {
            title: "运营商", align: 'center', width: 100, dataIndex: "isp",
```

`location` slot 的 tooltip 同样并进运营商分歧：

```html
                <template slot="location" slot-scope="text, row">
                    <a-tooltip v-if="row.locationAlt || row.ispAlt">
                        <template slot="title">
                            <template v-if="row.locationAlt">归属地：另一个源认为是 [[ row.locationAlt ]]<br></template>
                            <template v-if="row.ispAlt">运营商：另一个源认为是 [[ row.ispAlt ]]</template>
                        </template>
                        <span style="border-bottom: 1px dashed rgba(0,0,0,.25)">[[ row.location ]]</span>
                    </a-tooltip>
                    <span v-else-if="row.location">[[ row.location ]]</span>
                    <span v-else style="color: rgba(0,0,0,.25)">-</span>
                </template>
```

「近期来源」的 tooltip 补一行运营商：

```html
                        [[ src.location || '归属地未知' ]]<template v-if="src.locationAlt">（另一个源认为是 [[ src.locationAlt ]]）</template><br>
                        运营商 [[ src.isp || '未知' ]]<template v-if="src.ispAlt">（另一个源认为是 [[ src.ispAlt ]]）</template><br>
                        最后出现 [[ DateUtil.formatMillis(src.lastSeen) ]]，共 [[ src.count ]] 条记录
```

- [ ] **Step 6: 跑模板测试**

Run: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`
Expected: PASS。`getHtmlTemplate` 会吞掉 `ParseFS` 错误，光靠 `go build` 发现不了模板语法错误，这两个测试是唯一的守卫。

- [ ] **Step 7: 实机确认**

跑 `XUI_DEBUG=true go run main.go`（必须在仓库根目录，调试模式才从磁盘读模板），打开入站列表展开一行，确认：展开行没有出现横向滚动条；运营商列有值；两个源一致时不出现「存疑」。

如果本机没有 `bin/xray-darwin-arm64`，面板本身照常可访问，只是 xray 起不来——不影响这一列的验证。

- [ ] **Step 8: 提交**

```bash
git add web/html/xui/inbounds.html web/html/xui/access_log_modal.html
git commit -m "feat(ui): 入站展开行与访问日志显示来源 IP 的运营商"
```

---

### Task 8: 重新生成种子库并补真实数据断言

**Files:**
- Modify: `bin/ipdb.dat`（重新生成，仓库跟踪）
- Modify: `util/ipdb/realdata_test.go`

**Interfaces:**
- Consumes: Task 2/3 的 v2 格式与 ip2region 解析
- Produces: 无

- [ ] **Step 1: 写失败的测试**

在 `util/ipdb/realdata_test.go` 的 `TestRealDataLookupsKnownIPs` 里，把用例结构体加上 `isp` 字段并补断言（同一个测试里一起断言，不新开一个函数——它们查的是同一批 IP）：

```go
	for _, c := range []struct{ ip, country, region, city, isp string }{
		{"114.114.114.114", "中国", "江苏省", "南京市", "中国电信"},
		{"223.5.5.5", "中国", "浙江省", "杭州市", "阿里云"},
		{"202.96.209.5", "中国", "上海市", "上海市", "中国电信"},
		{"123.171.5.200", "中国", "山东省", "聊城市", "中国电信"},
		{"8.8.8.8", "United States", "", "", "Google"},
	} {
		loc, ok := db.Lookup(net.ParseIP(c.ip))
		if !ok {
			t.Errorf("Lookup(%s) 未命中", c.ip)
			continue
		}
		if loc.Country != c.country || loc.Region != c.region || loc.City != c.city || loc.ISP != c.isp {
			t.Errorf("Lookup(%s) = %q/%q/%q/%q, want %q/%q/%q/%q",
				c.ip, loc.Country, loc.Region, loc.City, loc.ISP,
				c.country, c.region, c.city, c.isp)
		}
	}
```

再补一条格式断言：

```go
// 种子库必须是当前格式。留一份 v1 在仓库里的后果是每台新装的机器都要等
// 一次几十 MB 的强制重下（Task 5 的判据会正确地触发它，但那是给存量部署的
// 补救，不该让新装的机器也走一遍）。
func TestRealDataIsCurrentFormat(t *testing.T) {
	if !realDB(t).HasISP() {
		t.Error("bin/ipdb.dat 还是不含运营商的旧格式，需要重新生成")
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./util/ipdb/ -run 'TestRealData' -v`
Expected: FAIL——`bin/ipdb.dat` 还是 v1，ISP 全为空

- [ ] **Step 3: 重新生成种子库**

项目里没有生成种子库的命令行工具，也不该为此新增一个（YAGNI：这是低频的发版动作）。用一次性程序生成，写在临时目录、用完删除：

```bash
SCRATCH=$(mktemp -d)
cat > "$SCRATCH/main.go" <<'EOF'
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"a-ui/util/ipdb"
)

func main() {
	resp, err := http.Get("https://raw.githubusercontent.com/lionsoul2014/ip2region/master/data/ipv4_source.txt")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create("bin/ipdb.dat")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	// builtAt 取整秒，与 fetchAndBuild 的 time.Now().Truncate(time.Second) 一致。
	if err := ipdb.Build(resp.Body, f, time.Now().Truncate(time.Second)); err != nil {
		log.Fatal(err)
	}
}
EOF
go run "$SCRATCH/main.go"
rm -rf "$SCRATCH"
```

（`go run` 一个仓库外的文件仍能 import `a-ui/util/ipdb`，因为工作目录在仓库根、module 是 `a-ui`。若 Go 版本拒绝这种用法，就把文件放到 `./tmpgen/main.go`，跑完连同目录一起删掉，并在提交前用 `git status` 确认它不在 diff 里。）

- [ ] **Step 4: 验证新库**

Run: `go test ./util/ipdb/ -v`
Expected: 全部 PASS，包括新的 ISP 断言与 `TestRealDataIsCurrentFormat`

同时确认体积符合预期：

```bash
ls -l bin/ipdb.dat   # 预期约 2.27 MB（升级前 2.18 MB）
```

- [ ] **Step 5: 全量门禁**

Run: `make verify`
Expected: vet + test + build 全绿

- [ ] **Step 6: 确认 diff 干净**

```bash
git status --short
git diff --stat
```

确认没有临时文件、没有无关改动。`bin/ipdb.dat` 是二进制，diff 只会显示 `Bin ... bytes`。

- [ ] **Step 7: 提交**

```bash
git add bin/ipdb.dat util/ipdb/realdata_test.go
git commit -m "chore(ipdb): 重新生成种子库为 v2 格式并补真实数据断言"
```

---

## 交付后自检

- [ ] `make verify` 全绿
- [ ] `git diff --stat` 里只有本计划涉及的文件，无调试残留、无临时脚本
- [ ] 实机确认过入站展开行与访问日志弹窗都能看到运营商列，且表格没有横向滚动条
- [ ] 设置页「IP 归属地库」显示的段数变化符合预期（ip2region 216,446 → 约 221,999）
- [ ] `docs/superpowers/specs/2026-09-07-ip-isp-column-design.md` 里的 §9 发布说明（升级后第一次更新会强制全量下载约 60 MB）已在发版说明中体现
