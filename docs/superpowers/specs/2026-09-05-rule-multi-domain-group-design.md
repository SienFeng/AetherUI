# 分流规则多域名组（复选框）设计文档

日期：2026-09-05
状态：待评审

> 本文修订 `2026-09-02-domain-routing-design.md` 的 §3.2（`RoutingRule` 表结构）、§5.3（第二道防线清单）与 `2026-09-05-routing-import-export-design.md` 的 `PortableRule` 结构。其余内容继续有效。
>
> 本文与 `2026-09-02-routing-multi-inbound-design.md` 是**同一个形状的第二次**：把分流规则上的一个单值外键搬成 JSON 数组。凡两者结论一致之处，本文直接引用而不重述理由；凡结论**相反**之处（主要是空数组的语义），本文逐条说明为什么必须相反。

## 1. 背景与目标

分流规则表单里，「用户（入站）」已经是复选框（多选），「域名组」仍是单选下拉框（`web/html/xui/routing.html:367`）。管理员维护了 Claude、ChatGPT 两个域名组、希望同一批用户的这两个组都走同一个落地节点时，必须建两条除域名组外完全相同的规则；此后每次调整出站节点或优先级都得逐条改。

目标：让一条分流规则可以同时引用多个域名组，交互与上方的「用户（入站）」完全对称。

生成期把选中的各组域名**合并成一份 `domain` 列表，输出一条 xray 规则**，而不是展开成多条。

### 非目标

- 不改动域名组自身的数据结构、订阅机制与校验
- 不引入域名组的嵌套 / 分组 / 继承
- 不改动「用户（入站）」一侧的任何语义
- 不改动出站节点、动作、优先级的语义
- 不做规则的批量编辑

## 2. 语义决策

三个决策点，均已与管理员确认：

**2.1 多选 = 合并成一条规则。** 勾选 Claude + ChatGPT，生成一条 `domain` 为两组并集的 xray 规则，而不是两条独立规则。备选方案「复选只是批量创建 N 条规则的快捷方式」被否决：编辑已有规则时会退回单选，交互不对称，且此后想同时改这几条仍要逐条操作。

**2.2 部分域名组失效 → 剔除失效的，剩下的照常生效。** 与现有「入站」一侧的处理完全对称（`routing_inject.go` 的 `buildRule` 剔除已删除/已禁用的入站并记 `logger.Warning`，剔完为空才整条丢弃）。

备选方案「只要有一个组失效就丢弃整条」延续了单组时的严格行为，被否决：一个订阅从未拉取成功的空组，会把同一条规则里本来好好的 Claude 分流一起废掉。对 `action=block` 的规则尤其糟糕——整条丢弃等于本该封禁的域名全部裸奔，而部分生成至少封住了还在的那部分。

**2.3 冲突判定的单位仍是「域名组 × 用户」的组合，不是域名组本身。** 一个域名组可以被任意多条规则引用，只要覆盖的用户不重叠。这条不变量本来就存在（`routing_rule.go:127`），本次只是从「组相同」推广成「组集合相交」，既没收紧也没放松。

| | 规则 1 | 规则 2 | 结果 |
|---|---|---|---|
| 同组不同人 | {Claude} × 甲 → A | {Claude} × 乙 → B | 允许 |
| 同组同人 | {Claude} × 甲 → A | {Claude} × 甲 → B | 拒绝 |
| 组集合部分重叠、同人 | {Claude, ChatGPT} × 甲 → A | {ChatGPT} × 甲 → B | 拒绝（ChatGPT × 甲 相交） |
| 组集合不重叠、同人 | {Claude} × 甲 → A | {ChatGPT} × 甲 → B | 允许 |
| 撞上「所有用户」 | {Claude} × 所有用户 → A | {Claude} × 甲 → B | 拒绝（全集与任何集合相交） |

拒绝的理由不变：xray 只命中优先级靠前的那条，后一条**永远不生效但界面上显示完全正常**。

## 3. 与多入站改造的关键分歧：空数组的语义相反

这是本次改造最容易出事、也最容易照抄错的一点。

| | `InboundIds`（既有） | `DomainGroupIds`（本次） |
|---|---|---|
| `[]` 的含义 | **合法**：对所有入站生效（含以后新建的） | **非法**：一律拒绝 |
| 生成期 `[]` 的后果 | 不输出 `inboundTag` 键，规则对所有入站生效——这正是想要的 | 若真输出，`domain` 为空数组，xray 视为「不限制」，规则从「这批域名走 B」**退化成「该用户全部流量走 B」** |
| xray 的反应 | — | `Configuration OK`，面板首页照常显示 `running` |

`2026-09-02-domain-routing-design.md` §2 的实测表里，「规则的 `domain` 为空数组 → Configuration OK，规则退化为匹配该入站全部流量」这一行就是本节的依据。因此：

- 编码函数的 Strict 版本，除了沿用「原始非空却编出 `[]` 就报错」，还要**加一条「原始本来就为空也报错」**——这与入站那边恰好相反，入站的空输入是用户通过「所有用户」复选框显式表达的合法语义，域名组没有对应的概念。
- 解码函数遇到空串 / `null` **不能**当作合法空集合返回，要让调用方把整条规则丢弃。入站那边把空串当 `[]` 是因为 `[]` 合法；这里不成立。

一句话：**`InboundIds` 的空数组是一等公民，`DomainGroupIds` 的空数组是数据损坏。** 任何一处照抄入站的空值处理都会打开「劫持该用户全部流量」这个洞。

## 4. 数据模型与迁移

### 4.1 字段

`database/model/routing.go` 的 `RoutingRule` 新增：

```go
// DomainGroupIds 是这条规则引用的域名组 id，JSON 整数数组，升序去重存储。
//
// 升序去重与 InboundIds 同理，是「生成逐字节确定」的一部分：buildRule 按
// 这个顺序逐组取域名再合并，顺序一抖动，Config.Equals 恒为 false，那个
// 10 秒的重启 cron 会不停重启 xray。
//
// 与 InboundIds 的空数组语义【相反】：这里的 [] 非法，绝不表示「所有域名
// 组」。域名条件为空会让 xray 把规则当作「不限制」，从「这批域名走 B」
// 退化成「该用户全部流量走 B」，且 Configuration OK、面板显示 running。
DomainGroupIds string `json:"domainGroupIds" form:"domainGroupIds"`
```

旧字段 `DomainGroupId int` **保留在结构体上、保留列**，不删。理由与 `inbound_id` 那次逐字同构（`database/db.go:61`）：管理员回退到旧版本二进制时，旧代码读到的还是原值，行为退回单选；删掉列则每条规则的 `domain_group_id` 都读成 0，`buildRule` 全部丢弃——分流静默全灭，而面板首页仍显示 `running`。

新代码一律只读写 `DomainGroupIds`，`DomainGroupId` 从此不再参与任何判断。

### 4.2 编解码

在 `web/service/routing_rule.go` 新增三个函数，与入站那组同形但空值策略相反：

- `EncodeDomainGroupIds(ids []int) (string, error)`：丢弃非正数、去重、升序、序列化。
- `EncodeDomainGroupIdsStrict(ids []int) (string, error)`：**写入路径唯一该用的版本**。编出 `[]` 一律报错，无论原始列表是否为空。
- `DecodeDomainGroupIds(encoded string) ([]int, error)`：空串 / `null` 返回空切片且**不报错**（迁移回填前、直接改库、并发写入都可能留下空值，在这里报错会让整份配置生成失败），交由 `validate` 与 `buildRule` 各自按空集合处理。真正的 JSON 语法错误仍返回 error。

### 4.3 迁移

紧贴 `database/db.go` 现有的 `migrateRoutingRuleInboundIds`，在 `initRouting()` 末尾追加一个同形函数：

```go
// 幂等：只回填 domain_group_ids 为空的行，面板每次启动都会跑。
// 全新安装的库没有 domain_group_id 列时先探测，避免 no such column。
UPDATE routing_rules
SET domain_group_ids = CASE WHEN domain_group_id > 0
                            THEN '[' || domain_group_id || ']' ELSE '[]' END
WHERE domain_group_ids IS NULL OR domain_group_ids = ''
```

`domain_group_id <= 0` 的脏数据（`validate` 挡得住表单，挡不住直接改库）回填成 `[]`，`buildRule` 会因「合并后域名为空」整条丢弃并记 `logger.Warning`——与现在 `domainGroupService.Get(0)` 失败后跳过整条的行为完全一致。**迁移不改变任何一条规则的实际生效范围。**

## 5. 冲突不变量

### 5.1 判定方式

`checkConflict`（`routing_rule.go:140`）现在靠 SQL 过滤同组规则：

```go
Where("domain_group_id = ? and id <> ?", rule.DomainGroupId, rule.Id)
```

JSON 数组列没法交给 SQL，改成读全部规则逐条解码，与 `CheckInboundRefs` 同形。`routing_rule.go:248` 已经为这个取舍写过理由（规则是几十条量级，这点开销换掉一张关联表是划算的），本次沿用，**不引入关联表**。

判定条件从「域名组相同 且 入站集合相交」推广为：

> **两条规则冲突，当且仅当「域名组集合相交」且「入站集合相交」。**

需要新增 `intersectGroups(a, b []int) (bool, int)`，**不能复用 `intersectInbounds`**：后者把空切片当全集（`len(a)==0 || len(b)==0` 直接返回 true），而域名组的空集合是非法值而非全集，复用会让两条各自损坏的规则被判成互相冲突，把管理员锁在门外。`intersectGroups` 是纯集合相交，无全集特例；第二个返回值取最小的相交 id，保证错误信息稳定可测。

### 5.2 错误文案

沿用现有句式，把撞上的那个**域名组**点名：

```
与分流规则「X」冲突：用户「甲」在域名组「Claude」下已被它覆盖。同一个用户在同一个域名组下只能有一条规则。
```

**「冲突」二字必须原样保留。** `routing_portable.go:748` 用 `strings.Contains(err.Error(), "冲突")` 把这类错误归进 `Skipped` 而不是 `Failed`，是导入幂等性的依赖；改文案前先同步看那一处（`routing_rule.go:169` 已有这条提醒）。

### 5.3 引用守卫

`CheckDomainGroupRefs`（`routing_rule.go:221`）同样从 SQL 计数改成逐条解码计数。

解码失败时**拦住删除**，与 `CheckInboundRefs` 完全一致：域名组的自增 id 同样会被 SQLite 复用（`primaryKey;autoIncrement` 生成的是 rowid 别名而非 `AUTOINCREMENT`）。删掉 Claude 组再新建 ChatGPT 组可能拿到同一个 id，孤儿规则会静默变成「ChatGPT 的域名走 Claude 的节点」——那时引用不再悬空，生成期的跳过防线拦不住，规则列表还会渲染得完全正常。

### 5.4 不做的事

冲突判定**只在写入路径生效，绝不在生成期干预**。迁移前写入的冲突数据照常生成两条规则，行为与本次改动前一致；生成期悄悄丢一条等于在管理员不知情时改变分流行为。历史冲突由界面标黄暴露，交给人决定改哪条。这条与 `2026-09-02-routing-multi-inbound-design.md` §5.3 一致。

## 6. 配置生成

`MergeDomains`（`routing_domain.go:291`）推广成变参：

```go
func MergeDomains(lists ...[]string) []string
```

现有的两参数调用点 `MergeDomains(manual, subscribed)` 在 Go 的变参规则下无需修改。

`buildRule`（`routing_inject.go:211`）的域名部分改为：

```
按 DomainGroupIds 升序遍历
  ├ 组不存在 / Domains 损坏 / SubscribedDomains 损坏 / 组内合并后为空
  │   → 剔除该组，logger.Warning（规则 id、组 id、原因）
  └ 否则 → 该组的 MergeDomains(manual, subscribed) 结果入列
跨组按遍历顺序合并去重，保留首次出现
  └ 合并后 len(domains) == 0 → 整条丢弃，说明原因
```

三点约束：

**「数据损坏」也走剔除，不走整条丢弃。** 它与「组为空」的后果完全相同（该组贡献 0 条域名），分开处理没有意义；且剔除的方向是缩小匹配范围，与 §2.2 的安全侧一致。

**跨组合并必须保序去重，禁止用 map 产生顺序。** 域名组落在 `RouterConfig` 这个 `json_util.RawMessage` 字段里，`Config.Equals` 按字节比较，所以**本次不需要扩展 `Config.Equals` / `InboundConfig.Equals`**——代价正是生成必须逐字节确定。顺序一抖动 `Equals` 恒为 false，`InboundController` 那个 10 秒 cron 会不停重启 xray。

**热应用路径不变。** 本次不新增任何 xray 配置段，routing 规则的变化本来就在 `ComputeHotDiff` 的热应用范围内（整体替换路由规则），`xray_hot_reload_e2e_test.go` 已覆盖。

## 7. 接口与前端

### 7.1 接口形态

`web/controller/routing.go`：

- `routingRuleForm.DomainGroupId int` → `DomainGroupIds []int`
- `routingRuleView.DomainGroupId int` → `DomainGroupIds []int`，并新增 `GroupsBroken bool`
- `ruleFromForm` 改走 `EncodeDomainGroupIdsStrict`

`GroupsBroken` 与既有的 `Broken`（标记 `InboundIds` 解码失败）分开，不合并成一个：两者的界面文案不同，合并会让「入站数据损坏」和「域名组数据损坏」显示成同一句话，管理员照着去修错的地方。

`DomainGroupIds` 解码失败时，view 里返回**空数组**（不能是 `null`，前端对它做 `.length`/`.includes`，`null` 会在渲染规则列表时抛异常，整页数据都出不来）——这一点与 `Broken` 那里的注释同理。所幸空的域名组数组在前端**没有**「所有域名组」这个歧义解读，渲染成红色的「域名组数据损坏」标签即可，不像入站那样需要靠标记位才能和合法状态区分开。

### 7.2 前端模型

`web/assets/js/model/routing.js` 的 `RoutingRule`：`domainGroupId = 0` → `domainGroupIds = []`，`fromJson` 用 `json.domainGroupIds || []` 兜底，新增 `groupsBroken`。

### 7.3 弹窗

`web/html/xui/routing.html:366-373` 那个 `<a-select>` 换成复选框列表，直接复用上方入站那套的结构与取舍：

- 域名组超过 5 个才出现搜索框（少的时候搜索框纯属噪音）
- 工具行显示「已选 N 组」+ 全选/清空
- 固定高度内部滚动，保证「保存」按钮永远留在视口里
- **域名组复选框本身永不禁用**，被禁用的是「用户」那一侧（沿用现有的「已被规则『X』覆盖」灰字提示）

`onDomainGroupChange` 改成 `toggleDomainGroup(groupId, checked)`。`routing.html:726` 那条注释解释的时序约束继续成立：必须在 `@change` 里显式赋值再读 computed，**不能改用 `watch`**——Vue 的 watcher 在 nextTick 异步触发，`openRule` 先换 rule 再把 visible 置真，等 watcher 跑起来 visible 已经是 true，守卫拦不住它，打开编辑弹窗会顺手把已勾选的项过滤一遍。

交互后果，需在实现时保持并在 UI 上可理解：**先勾用户、后勾域名组时，若某个已勾用户在新加的组下已被别的规则占用，该用户会被自动取消勾选。** 这是现有换组剔除逻辑的自然延续；不剔除的话，管理员会带着一个必被后端拒绝的选择去点保存。

`saveRule` 增加一道前端拦截：一个域名组都没勾时直接提示，不发请求。与 §3 一致——`[]` 在这里是数据损坏而非合法语义，前端没有理由把它发出去。

### 7.4 规则列表与告警

- 列表的「域名组」列：单个 `<a-tag>` → 多个，逐个用 `groupMissing` 判断是否标红
- `occupiedInbounds`：`r.domainGroupId !== gid` → 两条规则的组集合是否相交
- `ruleIssue`：域名组相关的两个分支（「域名组已删除」「域名组为空」）按 §6 的剔除语义重写——部分组失效不再是失效原因，只有**全部**组失效才是；另需新增 `groupsBroken` 的分支
- `ruleConflict`：`other.domainGroupId !== rule.domainGroupId` → 组集合相交判定，文案点名相交的组

## 8. 导入导出

### 8.1 格式

`routing_portable.go` 的 `PortableRule`：

```go
// DomainGroupRefs 是指针，理由与 InboundRefs 同构但【结论相反】：
// null / 键缺失 / 显式 [] 三者全部整条拒绝——域名组一个都没有等于 domain
// 条件为空。用指针不是为了区分放行与拒绝（都拒绝），而是为了让报错说得准：
// 是「字段缺失」还是「显式空数组」。
DomainGroupRefs *[]string `json:"domainGroupRefs"`
// DomainGroupRef 兼容 v1.7.0 及更早导出的文件，只读，不再作为主字段。
DomainGroupRef  string    `json:"domainGroupRef,omitempty"`
```

域名组仍用 `Remark` 作业务键，导出侧的 `checkDuplicateGroupRemarks` 硬拒绝与导入侧的重名歧义拒绝原样保留。

### 8.2 向后 / 向前兼容

**导出侧两个字段都写**：`domainGroupRefs` 永远写全；`domainGroupRef` 在**单组时**写组名、多组时留空。

这样新面板导出的文件放进旧面板：单组规则照常可用；多组规则会被旧面板的「`domainGroupRef` 为空 → 整条跳过」明确拒绝并计入 `Failed`。**明确报错远好过静默产生一条指向错误组的规则**——后者在规则表和生成的配置里都渲染得完全正常，只是流量走错节点，没有任何一层防线会发现。

**导入侧优先 `DomainGroupRefs`**，为 nil（对应 null 或键缺失）时回落 `DomainGroupRef`（非空则当作单元素数组），两者都拿不到内容则整条拒绝。

### 8.3 组认不出时的策略

与入站一侧对称：

- 空组名：整条拒绝（沿用 `routing_portable.go:656` 的既有理由——`groupByRemark[""]` 会在本机恰好存在一个备注为空的组时静默命中它）
- 重名歧义：整条拒绝（既有）
- **部分**组认不出：导入成**禁用**状态 + 把缺失的组点名报告
- **全部**组认不出：整条丢弃（编码结果会落回 `[]`，是 §3 定义的非法值）

「部分认不出 → 导入成禁用」相对现状是**行为变化**（现在单组时组不存在一律 `Failed`），换来的是与入站一致的心智模型：规则的其余部分都是好的，管理员打开编辑弹窗勾一下就能用。

`EncodeDomainGroupIdsStrict` 看的是解析后的 ids，全部认不出时它确实会报错（这与入站那边不同，入站的 Strict 在该路径上会安静返回 `"[]"`），但**仍要自己写那道「一个都没找到」检查**：报告文案要说清楚是「域名组在本机一个都没找到」，而不是把一个编码器的内部错误抛给管理员。

其余既有约定不变：冲突一律跳过绝不覆盖、逐条独立成败、不用事务、10MB 请求体上限、分项导出不隐式扩大范围。

## 9. 改动文件清单

| 文件 | 改动 |
|---|---|
| `database/model/routing.go` | `RoutingRule` 加 `DomainGroupIds`，保留 `DomainGroupId` |
| `database/db.go` | 加 `migrateRoutingRuleDomainGroupIds`，在 `initRouting()` 末尾调用 |
| `web/service/routing_rule.go` | 三个编解码函数、`intersectGroups`、`validate`、`checkConflict`、`CheckDomainGroupRefs` |
| `web/service/routing_domain.go` | `MergeDomains` 改变参 |
| `web/service/routing_inject.go` | `buildRule` 的域名合并与剔除 |
| `web/service/routing_portable.go` | `PortableRule` 结构、`toPortableRule`、`importRules` |
| `web/controller/routing.go` | `routingRuleForm` / `routingRuleView` / `ruleFromForm` / `getRules` |
| `web/assets/js/model/routing.js` | `RoutingRule` 模型 |
| `web/html/xui/routing.html` | 弹窗复选框、列表列、四处判定方法 |

测试文件的改动见 §10。

## 10. 测试策略

| 文件 | 用例 |
|---|---|
| `database/routing_migrate_test.go` | 回填 `domain_group_ids`；幂等（跑两次结果不变）；`domain_group_id = 0` 的脏数据回填成 `[]`；已有 `domain_group_ids` 的行不被覆盖 |
| `web/service/routing_rule_test.go` | `EncodeDomainGroupIdsStrict` 拒绝空输入与全非法输入；`intersectGroups` 不把空集合当全集；§2.3 表格五行逐行落成用例；`CheckDomainGroupRefs` 在解码失败时拦住删除 |
| `web/service/routing_inject_test.go` | 跨组合并去重保序；部分组失效剔除且规则仍生成；全部失效整条丢弃；**同输入两次生成结果逐字节相同**；空 `DomainGroupIds` 绝不输出空 `domain` |
| `web/service/routing_portable_test.go` | 新格式往返；旧格式（只有 `domainGroupRef`）能导入；多组导出时 `domainGroupRef` 为空；部分组认不出 → 禁用导入；全部认不出 → 丢弃；重跑幂等 |
| `web/html_test.go` | 既有的 `TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot` 跑通（新复选框在既有 modal 内，应不受影响） |

最后跑 `make verify`（vet + test + build），这是提交前的门禁。

## 11. 风险与边界

**回退到旧版本二进制**：`domain_group_id` 列保留，旧代码读到的是迁移前的原值，单组规则行为完全正常；本次改造后新建的**多组**规则，旧代码只会看到 `domain_group_id` 的旧值（多组规则该值为 0）→ `buildRule` 整条丢弃 + Warning。即分流范围缩小而非放大，安全侧正确。这一点应写进 CLAUDE.md。

**照抄入站空值处理**是本次改造唯一的高危失误模式，§3 已单独立节。任何一处把 `DomainGroupIds` 的 `[]` 当作合法语义放行，都会打开「劫持该用户全部流量」这个洞，且 xray 返回 `Configuration OK`、面板首页显示 `running`，没有任何一层会报错。

**生成确定性**：跨组合并引入了一层新的顺序依赖。若实现时图省事遍历 map 产生顺序，症状不是报错而是 xray 每 10 秒重启一次，排查方向容易跑偏到 `CheckXrayRunningJob`。测试里那条「同输入两次生成逐字节相同」是专门守这个的。

**已有单组规则的用户零感知**：迁移把每条规则回填成单元素数组，界面上表现为「已选 1 组」且那一组被勾中，生成的配置逐字节不变（单组时跨组合并退化成恒等）——因此**升级不会触发一次无谓的 xray 重启**。
