# 分流规则多入站（复选框）设计文档

日期：2026-09-02
状态：待评审

**本文档修订 `2026-09-02-domain-routing-design.md` 的 §3.2（`RoutingRule` 表结构）、§3.3（`InboundId` 相关的两条设计点）与 §5.3（引用完整性防线）。** 未被本文档提及的部分一律继续有效——尤其是「一律 append 到末尾」「block 规则排在 proxy 之前」「生成逐字节确定」「绝不输出条件残缺的规则」这四条不变量。

## 1. 背景与目标

分流规则当前只能绑定**一个**入站（`InboundId`，0 表示所有入站）。管理员想让甲、乙、丙三个用户的 ChatGPT 流量都走东京节点，必须建三条内容重复的规则；规则表会随用户数线性膨胀，改一次落地节点要逐条改。

同时，现有模型**允许**同一个用户在同一个域名组下被两条规则以不同出口覆盖：

```
规则 1：甲 + ChatGPT 组 → 东京节点
规则 2：甲 + ChatGPT 组 → 新加坡节点
```

两条都会写进配置，xray 按顺序取先匹配的那条，后一条静默失效。「甲的 ChatGPT 到底走哪」需要读优先级才能回答，规则表本身不足以说明。

本设计要达成两件事：

1. 一条规则可以覆盖多个入站，界面上用复选框勾选。
2. **同一个域名组下，任何一个入站至多被一条规则覆盖**——从数据模型层面消除上面那种歧义。

### 非目标

- 不改「一个入站 = 一个用户」的维度选择（见原设计 §3.1）
- 不做规则的批量导入导出
- 不自动修改已有的冲突数据（见 §2 决策三）

## 2. 语义决策

三条决策已与管理员确认，是后续所有实现的前提。

**决策一 · 「所有用户」与「指定用户」严格互斥。**
一个域名组一旦有了「所有用户」规则，就不能再对它添加任何指定用户的规则；反之，已有指定用户的规则时，「所有用户」也不能勾选。

被否决的方案是「允许共存、靠优先级决定谁先命中」。它保留了「默认走 X、甲例外走 Y」的写法，但代价正是本设计要消除的东西：单看规则表无法回答「甲的 ChatGPT 走哪」。需要兜底 + 例外时，改成「勾上除甲以外的所有人」即可表达，且表达是显式的。

**决策二 · 禁用的规则同样占位。**
冲突判定不看 `Enable`。这样「保存时没问题、一启用才发现撞车」的情况不存在，管理员想腾位置就得先改掉或删掉旧规则。代价是不能预先建好一批互斥规则轮换启用——这不是当前需求。

**决策三 · 已有的冲突数据不自动处理。**
迁移只做字段搬运，绝不改变任何一条规则的实际生效范围。迁移后可能存在违反新不变量的历史数据（`Add`/`Update` 拦不到它们，它们是迁移前写入的）。处理方式是**在规则列表里标黄并指出与哪条规则冲突**，由管理员决定改哪条。

自动禁用后建的那条被否决：那会在管理员不知情的情况下改变实际分流行为，一条 block 规则被自动禁用意味着那批域名当场恢复直连。

## 3. 可行性验证

以下结论由本机 Xray-core **26.7.28** 实测得出，非推断。`-test` 部分用 `xray run -test -c`，运行时部分实际起进程、用 `curl --socks5-hostname` 走不同入站验证。

| 验证项 | 结果 |
|---|---|
| `inboundTag` 含多个 tag | `Configuration OK` |
| `inboundTag` 含多个 tag，运行时匹配范围 | **只对列出的入站生效**：规则写 `["inbound-10801","inbound-10802"]` 时，这两个入站访问目标域名被 blackhole 掐断，第三个入站 `inbound-10803` 正常返回 200 |
| `inboundTag: []`（空数组） | `Configuration OK` |
| `inboundTag: []`，运行时匹配范围 | **对所有入站生效**：两个入站访问目标域名都被掐断，对照域名正常 200 |

第一、二行确认多入站方案本身可行，是本设计的基础。

**第三、四行是本设计最重要的输入**，它确认了一个新的事故面：空的 `inboundTag` 不会被 xray 拒绝，而且语义是「不限制」而非「不匹配任何入站」。这与原设计 §2 中 `domain: []` 的行为完全同构——xray 的路由条件是 AND 关系，空条件不参与匹配，于是整条规则的作用域被放大到全体。

具体到本功能：一条本该只覆盖甲的规则，如果甲的入站被删除或禁用而代码天真地生成 `inboundTag: []`，规则会**劫持所有人**的这批域名流量，且 `Configuration OK`、面板首页显示 running、无任何报错。§6 是为堵这个洞而写的。

## 4. 数据模型与迁移

### 4.1 字段

```go
type RoutingRule struct {
    Id     int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
    Remark string `json:"remark" form:"remark"`
    // InboundIds 是这条规则覆盖的入站 id，JSON 整数数组，升序去重存储。
    // 空数组 [] 表示「所有用户」（含以后新建的入站）。
    // 升序去重不是洁癖：它是「生成逐字节确定」不变量的一部分，
    // 也让 §5 的冲突判定不必考虑重复元素。
    InboundIds    string `json:"inboundIds" form:"inboundIds"`
    DomainGroupId int    `json:"domainGroupId" form:"domainGroupId"`
    Action        string `json:"action" form:"action"`
    OutboundId    int    `json:"outboundId" form:"outboundId"`
    Priority      int    `json:"priority" form:"priority"`
    Enable        bool   `json:"enable" form:"enable"`
}
```

`InboundId` 字段从结构体中删除。

**为什么是 JSON 数组列而不是关联表。** 备选方案是新建 `routing_rule_inbounds(rule_id, inbound_id)` 关联表。否决理由：规则量级是几十条，全表扫描加内存过滤绰绰有余；而关联表会多出一张表、一条新的引用边、以及删除时的事务需求——本项目目前没有事务模式，引入它的成本高于收益。JSON 字符串数组是本子系统已有的模式（`DomainGroup.Domains`、`SubscribedDomains`），沿用它不新增概念。

已知代价：`CheckInboundRefs` 不能再用 `WHERE inbound_id = ?` 让 SQLite 去数，必须读出全部规则逐条解码判断。见 §7.2。

第三个备选方案「不改模型，前端多选时生成 N 条规则」直接否决：编辑一次规则要 diff 出增删改三类操作，规则列表随用户数膨胀，而本设计的冲突约束反而更难落地。

### 4.2 编解码

在 `web/service/routing_rule.go` 中新增一对函数，与 `routing_domain.go` 的 `EncodeDomains` / `DecodeDomains` 保持同样的职责划分：

- `EncodeInboundIds(ids []int) (string, error)`：过滤掉非正数、去重、**升序排序**后编码。
- `DecodeInboundIds(raw string) ([]int, error)`：空串与 `"null"` 一律当作空数组（= 所有用户），解码失败返回错误。

空串必须容错：迁移虽然会回填，但直接改库、并发写入等路径仍可能留下空值，而在这里 panic 或报错会让整份配置生成失败。

**非空输入绝不能被静默过滤成空数组。** `EncodeInboundIds` 会丢掉非正数，于是 `[0]`、`[-1]` 这类输入会得到 `[]`——而空数组的语义是「所有用户」。一次前端 bug 或手工构造的请求，就能把一条本该只覆盖某个人的规则悄悄放大到全体，且一路合法。因此写入路径（§7.1 的 controller 转换处）必须多一道判断：**原始数组非空、过滤后为空 → 报错拒绝**，不允许降级为「所有用户」。「所有用户」只能来自前端那个显式的复选框，即原始数组本来就是空的情况。

### 4.3 迁移

放在 `database.initRouting()` 里，`AutoMigrate(&model.RoutingRule{})` 之后：

```sql
UPDATE routing_rules
SET inbound_ids = CASE WHEN inbound_id > 0 THEN '[' || inbound_id || ']' ELSE '[]' END
WHERE inbound_ids IS NULL OR inbound_ids = ''
```

三个要点：

1. **幂等**。`WHERE` 条件保证已迁移的行不会被再次覆盖，重启多少次都安全。
2. **新装库要跳过**。全新安装的库没有 `inbound_id` 列，这条 SQL 会报 `no such column`。执行前用 `db.Migrator().HasColumn(&model.RoutingRule{}, "inbound_id")` 守住——字段已从结构体删除，GORM 会直接把传入的字符串当列名去查，正是我们要的行为。
3. **`inbound_id` 列保留不删**。GORM 的 SQLite AutoMigrate 本来就不删列，这里是顺水推舟：万一管理员回滚到旧版本二进制，旧代码读到的 `inbound_id` 还是原值，行为退回单选而不是全部变成「所有用户」——后者会把每一条规则的作用域悄悄放大到全体。

## 5. 冲突不变量

> **同一个域名组下，任何一个入站至多被一条规则覆盖。**

### 5.1 判定方式

把每条规则看作它覆盖的入站集合 `S(rule)`：

- `InboundIds` 为空数组 → `S` = 全集（含以后新建的入站）
- 否则 → `S` = 那些 id 的集合

两条规则冲突，当且仅当 `DomainGroupId` 相同且 `S₁ ∩ S₂ ≠ ∅`。全集与任何集合相交，也与另一个全集相交——决策一的「严格互斥」就是这条判定的自然结果，不需要额外分支。

不同域名组之间永不冲突，即使域名内容有重叠。那种重叠由 `Priority` 决定先后，是既有语义，本设计不动。

### 5.2 落点

`RoutingRuleService.checkConflict(rule)`，由 `Add` 与 `Update` 在现有的 `validate` 之后调用：

- 取出同 `DomainGroupId` 的全部规则，**不过滤 `Enable`**（决策二）
- `Update` 时排除 `rule.Id` 自身
- 冲突时报错必须点名到人和规则：`用户「甲」在域名组「ChatGPT」下已被规则「甲的 ChatGPT 走 B」覆盖`。只说「存在冲突」等于让管理员自己去翻规则表

前端会把已占用的用户置灰（§7.3），但那只是体验。**后端这一层才是防线**——直接调接口、并发提交、前端数据过期都绕不过它。

### 5.3 不做的事

`checkConflict` 只在写入路径生效，**不在生成期干预**。迁移前写入的冲突数据照常生成两条规则，xray 取先匹配的那条，与本功能上线前的行为完全一致。理由见决策三：生成期悄悄丢弃一条规则，等于在管理员不知情时改变分流行为，而这正是本子系统一贯要避免的静默失效。

历史冲突通过界面标黄暴露给管理员，由人来决定改哪条。

## 6. 配置生成

`RoutingInjector.buildRule` 中，原来的：

```go
if rule.InboundId > 0 {
    tag, ok := inboundTagById[rule.InboundId]
    if !ok {
        return nil, false, common.NewError("入站不存在或已禁用, id:", rule.InboundId)
    }
    generated["inboundTag"] = []string{tag}
}
```

改为：解码 `InboundIds`（失败则整条跳过），非空时逐个查 tag，查不到的剔除并记录，然后：

```go
if len(tags) == 0 {
    // 剩下空数组绝不能输出。§3 实测确认 xray 把 inboundTag: [] 当作
    // 「不限制」而非「不匹配任何入站」——一条本该只覆盖甲的规则会
    // 劫持所有人的这批域名，且 Configuration OK、面板显示 running、
    // 无任何报错。这与 domain 为空数组是同一类事故。
    return nil, false, common.NewError("规则指定的入站全部不存在或已禁用, ids:", ids)
}
if len(tags) < len(ids) {
    // 部分失效不整条丢弃：剩下的入站仍应按规则走。但必须记 warning，
    // 否则被剔除的那些用户会静默回落直连而无人察觉。
    logger.Warning(...)
}
generated["inboundTag"] = tags
```

**「部分失效保留、全部失效丢弃」是本节的核心取舍。** 整条丢弃会让无辜的用户也失去规则；而输出空数组会把规则放大到全体——后者严重得多，所以边界情况一律往「丢弃」倒。

`tags` 的顺序由 `InboundIds` 的升序保证（§4.2），满足「生成逐字节确定」不变量。**不得**遍历 `inboundTagById` 这个 map 来产生数组顺序。

原设计 §5.3「第二道防线」的清单据此更新为：域名组不存在或合并后域名为空、出站不存在或已禁用、**规则指定的入站全部不存在或已禁用**——一律整条跳过并记 warning。

## 7. 接口与前端

### 7.1 接口形态

前端不应该碰 JSON 字符串。按 `domainGroupForm` 的既有做法，在 controller 层加一层转换：

```go
type routingRuleForm struct {
    Id            int    `json:"id" form:"id"`
    Remark        string `json:"remark" form:"remark"`
    InboundIds    []int  `json:"inboundIds" form:"inboundIds"`
    DomainGroupId int    `json:"domainGroupId" form:"domainGroupId"`
    Action        string `json:"action" form:"action"`
    OutboundId    int    `json:"outboundId" form:"outboundId"`
    Priority      int    `json:"priority" form:"priority"`
    Enable        bool   `json:"enable" form:"enable"`
}

type routingRuleView struct {
    Id            int    `json:"id"`
    Remark        string `json:"remark"`
    InboundIds    []int  `json:"inboundIds"`
    DomainGroupId int    `json:"domainGroupId"`
    Action        string `json:"action"`
    OutboundId    int    `json:"outboundId"`
    Priority      int    `json:"priority"`
    Enable        bool   `json:"enable"`
    // Broken 标记 InboundIds 列解码失败。这种规则 buildRule 会整条丢弃，
    // 但解码失败得到的空数组在前端看来就是「所有用户」——不带这个标记，
    // 一条已经不生效的规则会在界面上显示成覆盖全员的正常规则。
    Broken bool `json:"broken"`
}
```

`list` 接口返回 `routingRuleView` 而非裸 model，`InboundIds` 解码成数组。解码失败时返回空数组并把 `Broken` 置真——与 `domainGroupSummary.Broken` 同样的理由：不能让前端把「数据损坏」和「所有用户」混为一谈。

### 7.2 引用守卫

`CheckInboundRefs(inboundId)` 改为读出全部规则、解码 `InboundIds`、判断是否包含该 id。语义保持不变：

- 空数组（所有用户）**不算**引用某个具体入站——与原来 `InboundId = 0` 不参与检查一致
- 命中则拒绝删除入站，不自动把该 id 从规则里摘掉

不自动摘除是有意的：一条覆盖 `[甲, 乙]` 的规则被摘成 `[乙]` 是一次静默的作用域收缩，而 SQLite 的 id 复用意味着新建的入站还可能捡到甲的旧 id。维持「拒绝删除、由管理员显式处理」的现有姿态。

### 7.3 弹窗

```
用户（入站）
  [ ] 所有用户（含以后新建的入站）
  ─────────────────────────────
  [x] 甲
  [ ] 乙   已被规则「乙的 ChatGPT 走 C」覆盖   ← 置灰不可勾
  [ ] 丙
```

- 「所有用户」是一个独立的 `a-checkbox`，勾选后下方整组置灰。它不混进 `a-checkbox-group` 里——混进去就需要一堆互斥逻辑，而且「全集」和「某个具体用户」本来就不是同一层的东西。
- 占用集合按当前选中的**域名组**实时计算，编辑时排除自身。
- 域名组有 `watch`：切换域名组要重算占用集合，并把已勾选但在新域名组下不可用的项剔除掉，否则会带着一个必被后端拒绝的选择去点保存。
- 同域名组已存在「所有用户」规则时，「所有用户」和下方所有用户项全部置灰，并给出说明。

### 7.4 规则列表

- 「用户」列渲染多个 tag：空数组显示「所有用户」，否则逐个渲染，已删除的入站标红。超过 3 个时只渲染前 3 个再加一个「等 N 人」，避免撑破表格；完整名单在 tooltip 里给出。
- `ruleIssue` 扩展为覆盖以下几类静默失效：
  1. 域名组已删除 / 为空（既有）
  2. 出站已删除 / 已禁用（既有）
  3. **指定的入站全部失效** → 整条不会写进配置
  4. **部分入站失效** → 这些用户不受规则约束（既有逻辑只处理单入站）
  5. sniffing 未开启 → 逐个入站检查，列出不满足的那些（既有逻辑只处理单入站）
  6. **`InboundIds` 数据损坏**（`broken` 为真）→ 整条不会写进配置，且必须与「所有用户」区分开
  7. **与其他规则冲突**（历史数据）→ 指出与哪条规则、在哪个用户上撞了

## 8. 改动文件清单

| 文件 | 改动 |
|---|---|
| `database/model/routing.go` | `RoutingRule.InboundId` → `InboundIds`；注释说明空数组语义 |
| `database/db.go` | `initRouting` 增加一次性回填迁移，`HasColumn` 守卫 |
| `web/service/routing_rule.go` | `EncodeInboundIds`/`DecodeInboundIds`；`checkConflict`；`Update` 字段搬运；`CheckInboundRefs` 改判定方式 |
| `web/service/routing_inject.go` | `buildRule` 多入站生成与「全部失效则丢弃」 |
| `web/controller/routing.go` | `routingRuleForm` / `routingRuleView`；`addRule`/`updateRule`/`listRules` |
| `web/assets/js/model/routing.js` | `RoutingRule.inboundIds` 数组 |
| `web/html/xui/routing.html` | 弹窗复选框、占用计算、列表渲染、`ruleIssue` 扩展 |
| `docs/superpowers/specs/2026-09-02-domain-routing-design.md` | 在 §3.2/§3.3/§5.3 标注被本文档修订 |
| `CLAUDE.md` | 「规则存 `InboundId` 外键」「`InboundId = 0` 表示全局」两条描述同步更新 |

## 9. 测试策略

**`web/service/routing_inject_test.go`**

- 多入站生成 `inboundTag` 为多元素数组，顺序与 `InboundIds` 升序一致
- 部分入站不存在 / 已禁用 → 只保留有效的，规则仍输出
- **全部入站不存在 / 已禁用 → 整条跳过，配置里绝不出现 `inboundTag: []`**（本次最重要的一条）
- 空数组（所有用户）→ 不输出 `inboundTag` 键
- `InboundIds` 数据损坏 → 整条跳过
- 生成两次字节完全一致

**`web/service/routing_rule_test.go`**

冲突判定六个分支：个体×个体相交、个体×个体不相交（放行）、全局×个体、个体×全局、全局×全局、不同域名组（放行）；外加禁用规则同样占位、`Update` 排除自身。

`EncodeInboundIds` / `DecodeInboundIds`：去重升序、空串与 `"null"` 解码为空数组、损坏输入报错；以及**非空输入全是非法 id 时写入路径报错而非降级为「所有用户」**（§4.2）。

**`database`**

迁移回填：`inbound_id > 0` → `[n]`、`= 0` → `[]`、已有值不被覆盖、重复执行幂等。

**`CheckInboundRefs`**

覆盖多入站规则中间的一个 id、空数组规则不算引用。

**模板**

`web/html_test.go` 的 `TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot` —— 弹窗改动必须跑，Vue 指令写在根元素外是完全静默的死代码。

## 10. 风险与边界

**改了 `web/assets/js` 但版本号没变会命中强缓存。** `cur_ver` 取自 `config.GetVersion()`，本地验证时需要硬刷新，部署时靠发版 tag 更新版本号。

**历史冲突数据在界面上是黄色警告，不是错误。** 管理员不理会的话，行为与现在完全一致（按优先级先匹配者生效），不会变得更糟，但也不会自动变好。

**「所有用户」的语义是动态的。** 它包含以后新建的入站，这正是全局封禁规则需要的性质；反过来说，勾选具体用户的规则不会自动覆盖新用户。这个区别要在界面文案里说清楚，否则管理员会以为「勾上当前全部用户」等价于「所有用户」。

**入站数量很多时复选框会很长。** 当前部署规模（个位数到几十个用户）下可接受，暂不做搜索框和分页。若将来入站上百，再加过滤输入框。
