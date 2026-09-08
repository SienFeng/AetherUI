# 分流规则自动应用于新增入站 设计文档

日期：2026-09-07
状态：待评审

> 本文修订 `2026-09-02-domain-routing-design.md` 的 §3.2（`RoutingRule` 表结构）与 §5（两道防线）、`2026-09-02-routing-multi-inbound-design.md` 的写入路径清单、`2026-09-05-routing-import-export-design.md` 的 `PortableRule` 结构。其余内容继续有效。

## 1. 背景与目标

一条分流规则要覆盖「除某几个人之外的所有用户」时，现在只能把那些人逐个勾上。生产实例上的实际形态：ChatGPT 域名组下有一条只给 XyQin 的规则（走 IProyal），另一条要覆盖其余 16 人（走新加坡）——那 16 个人是一个一个勾出来的。

代价不在这一次勾选，而在**此后每新建一个入站**：管理员要回到分流页，把新用户挨条勾进 ChatGPT、Claude、Gemini、Cursor…… 漏勾一条，那个用户访问对应域名时静默走默认出站——规则表渲染正常，配置生成正常，xray 返回 `Configuration OK`，没有任何一层会报错。这正是本子系统反复防范的失效形状，而这次的成因是纯粹的人工遗漏。

管理员不能直接选「所有用户」，是因为 `checkConflict` 的核心不变量：**同一个域名组下，任何一个入站至多被一条规则覆盖**。`intersectInbounds` 把空数组当全集，`[]` 与任何具体用户规则必然相交。

目标：让一条列了具体用户的规则可以声明「以后新建的入站也自动应用此规则」，**不放宽上述不变量**。

### 非目标

- 不改动 `InboundIds` 空数组的语义（仍是「所有用户」）
- 不放宽 `checkConflict` 的既有判定，不引入靠规则顺序解决重叠的模型
- 不引入「排除列表」语义（理由见 §2.2）
- 不改动域名组、出站节点、动作、优先级的任何语义
- 不改动删除入站时的引用守卫行为（理由见 §8）

## 2. 语义决策

### 2.1 写入期扩散，不是生成期推导

新建入站时，把它的 id 追加进所有勾了标记的规则的 `InboundIds`。数据保持静态。

备选方案「生成期推导」（`buildRule` 时动态算出这条规则该覆盖谁）被否决：**规则弹窗里勾选框显示的名单会和实际生效的名单不一致**。管理员打开规则看到 16 个人，实际生效 17 个——这是本项目最忌讳的形状。而且 `checkConflict` 也得跟着动态推导，判定复杂度随之上升。

写入期扩散的另一个好处是 `buildRule` / `checkConflict` / 配置生成 / 导入导出 / 回退契约**一个字都不用改**：它们看到的永远是一份普通的具体 id 列表。

### 2.2 为什么不做「排除列表」

「所有用户，但排除 X」（`InboundIds=[]` + `ExcludedInboundIds=[X]`）语义上更直接，但**回退契约是致命的**：旧二进制不认识排除列，读到 `InboundIds=[]` 就当成纯粹的「所有用户」，X 被卷进来走了本不该走的节点。规则覆盖范围被**静默放大**，方向与本子系统「宁可缩小不可放大」的安全侧原则正相反，且没有任何一层会报错。

本方案的回退方向相反：旧二进制忽略新列，规则就是 `inbound_ids` 里列着的那些人——**范围缩小**，安全侧正确。

### 2.3 默认值：后端零值 false，前端新建表单默认勾上

两者是不同的东西，不能合并：

- **后端零值 `false`**。AutoMigrate 给老库加上这一列之后，没有任何规则会自动扩散，升级后行为零变化。导入的旧文件、直接调接口创建的规则同样落在 `false`。
- **前端新建规则表单默认勾上**。绝大多数分流规则本来就该覆盖所有人；只给特定几个人的规则（如 fse1204 那条）由管理员手动取消。

**已有规则不做迁移标记。** 升级时一条都不置 `true`：自动标记会让下一个新建入站突然被卷进全部 7 条规则，而管理员没有主动要求过。需要的规则由管理员逐条勾上（几分钟的事）。

## 3. 数据模型

`model.RoutingRule` 增一列：

```go
// ApplyToNewInbounds 为真时，以后新建的入站会在创建它的同一个事务里被追加
// 进这条规则的 InboundIds（见 RoutingRuleService.AttachInbound）。
//
// 这是写入期扩散而非生成期推导：数据保持静态，规则弹窗里勾选框显示的就是
// 实际生效的名单。推导方案会让两者不一致，见设计 §2.1。
//
// 零值 false：AutoMigrate 给老库加上它之后没有任何规则会自动扩散，
// 升级后行为零变化。前端新建表单默认勾上，那是表单初始值，不是这里的默认值。
ApplyToNewInbounds bool `json:"applyToNewInbounds" form:"applyToNewInbounds"`
```

列名 `apply_to_new_inbounds`。GORM 的 sqlite AutoMigrate 只加列不删列，因此回退安全。

## 4. 扩散机制

### 4.1 `AttachInbound`

```go
// AttachInbound 把新建的入站追加进所有声明了自动应用的规则。
// tx 由调用方传入——扩散必须与建入站在同一个事务里，见 §4.3。
func (s *RoutingRuleService) AttachInbound(tx *gorm.DB, inboundId int) error
```

逐条处理 `apply_to_new_inbounds = true` 的规则，把 `inboundId` 追加进 `InboundIds` 并走 `EncodeInboundIds`（升序去重——那是「生成逐字节确定」的前提，顺序一抖动 `Config.Equals` 恒为 false，10 秒的重启 cron 会不停重启 xray）。

### 4.2 `InboundIds` 为空数组的规则必须跳过

**这是本方案唯一的高危失误模式。**

空数组表示「所有用户」。往里追加一个 id，规则会从「覆盖所有人」**降级成「只覆盖这一个人」**——其余 18 个人当场失去这条规则，静默退回默认出站。xray 返回 `Configuration OK`，面板显示 `running`，规则表看起来只是「用户」列少了一堆标签，而管理员刚做的操作是「新建了一个入站」，不会往这里想。

跳过是无损的：空数组已经覆盖全部入站，包括刚建的这一个。

正常情况下勾了标记的规则不会是空数组（§5.2 的表单联动保证了这一点），但**这道判断不能因此省掉**：直接改库、导入的文件、将来某条新写入路径都可能留下这种组合，而它的后果比「标记不生效」严重得多。

### 4.3 调用点：三条建入站路径，且必须在事务内

| 路径 | 入口 | 说明 |
|---|---|---|
| 面板新建入站 | `web/controller/inbound.go` → `InboundService.AddInbound` | 主场景 |
| 安装向导建 REALITY 入站 | `bootstrap/bootstrap.go` → `InboundService.AddInbound` | 新装时通常还没有规则，扩散是 no-op |
| v2-ui 迁移 | `v2ui/v2ui.go` → `InboundService.AddInbounds` | 批量，逐个 Attach |

只挂 `AddInbound` 会漏掉第三条。扩散调用收在 service 层，两个方法都调。

`AddInbound` 现在是裸的 `db.Save(inbound)`，改为事务：先 Save 拿到自增 `Id`（Attach 需要它），再 Attach，任一步失败整体回滚。

**为什么必须回滚。** 扩散失败若只记日志放行，结果就是一个用户静默地没进规则——正是本功能要消灭的那种失效，而且比人工遗漏更隐蔽（管理员以为系统已经处理了）。宁可让建入站整个失败并报清原因，让他重试。

`inbounds` 与 `routing_rules` 同在主库，一个事务能盖住两者。


### 4.4 不触发扩散的时刻

- **启用 / 禁用入站**：禁用的入站仍留在 `InboundIds` 里，`buildRule` 生成时会剔除已禁用的，重新启用即恢复，与本功能无关。
- **编辑入站**（改端口、备注、到期时间等）：入站身份没变。
- **回退期间新建的入站**：旧二进制不执行扩散，重新升级后**也不会补**——扩散只发生在建入站的那一刻。这些入站需要管理员手工勾进规则。

### 4.5 重启标志无需另加

扩散改动了规则的 `InboundIds`，配置随之改变。但三条路径本身都已经处理了：面板新建走 `InboundController.addInbound`，成功后就调 `xrayService.SetToNeedRestart()`（`web/controller/inbound.go:92`）；`bootstrap` 与 `v2ui` 运行时 xray 尚未由面板托管。`AttachInbound` 内部**不要**再调一次——service 层置重启标志与本项目「controller 负责触发、service 保持无状态」的分层不符。

## 5. 冲突不变量

### 5.1 新增判定

`checkConflict` 增一条：**对规则引用的每一个域名组，至多允许一条声明了自动应用的规则引用它。**

判定单位是**域名组**，不是规则——一条规则可以引用多个域名组，两条规则只要在任一组上都声明了自动应用即冲突。

不加这条会让系统自己造出违反核心不变量的数据：新建入站时它被同时扩散进两条规则，这两条随即在同一域名组下覆盖同一个入站，而管理员什么都没做错。既有的 `checkConflict` 在写入时拦不住它——那次写入是 `AttachInbound` 发起的，不走表单校验。

### 5.2 与「所有用户」的联动

`InboundIds` 为 `[]` 时这个标记天然为真（空数组已含未来新建）。表单选中「所有用户」时前端自动勾上并禁用该复选框，不让管理员配出「所有用户 + 不应用于新增」这种自相矛盾的组合。

后端不做强制改写：`[]` 的规则无论该列是 true 还是 false，扩散都会按 §4.2 跳过它，行为完全一致。强制改写只会引入一个必须与 `InboundIds` 保持同步的派生值。

### 5.3 错误文案

文案必须包含「冲突」二字：`routing_portable.go` 的 `importRules` 用 `strings.Contains(err.Error(), "冲突")` 把这类错误计入 `Skipped` 而非 `Failed`，导入才能保持幂等。导入一条声明自动应用的规则而本机已有另一条声明了同组的，确实属于「本机已存在同覆盖范围的规则」。

拟：

```
与分流规则「%s」冲突：域名组「%s」下已有一条声明了「以后新增用户自动应用」的规则。
同一个域名组下只能有一条规则自动纳入新用户。
```

### 5.4 `Update` 必须同步这个字段——与 `priority` 相反

`RoutingRuleService.Update` 里 `priority` 是**刻意不同步**的（表单没有这一项，照抄零值会把规则弹到列表顶部）。这个新字段的结论相反：表单里确实有这个复选框，管理员改了就该落库，因此 `old.ApplyToNewInbounds = rule.ApplyToNewInbounds` 必须写。

两个相邻字段一个同步一个不同步，是实现时最容易照着隔壁抄错的地方，两侧都要留注释。

### 5.5 取消勾选不回滚已扩散的成员

管理员取消勾选后，此前被自动加进来的入站**留在 `InboundIds` 里**，不会被移除。它们已经是这份名单的一部分，回滚需要记住「哪些是自动加的、哪些是手工勾的」——那是一份没人要求过的额外状态，而且回滚会静默缩小规则范围。要移除请手工取消勾选那个人。

## 6. 接口与前端

### 6.1 接口

`routingRuleForm` 与 `routingRuleView` 各加一个 `applyToNewInbounds` 布尔字段。不新增接口。

### 6.2 前端模型

`web/assets/js/model/routing.js` 的 `RoutingRule` **构造函数与 `fromJson` 两处都要加**这个字段。只加一处，服务端返回的值会被静默丢弃，界面永远显示构造函数里那个硬编码的初始值。

### 6.3 规则弹窗

「用户（入站）」区、搜索框上方加：

```
☑ 以后新增的用户自动应用此规则
```

新建时默认勾上；选中「所有用户」时自动勾上并禁用（§5.2）。

### 6.4 规则列表

「用户」列在用户标签之后追加一个标记（如 `+新用户`）。不显示的话，管理员从列表完全看不出哪些规则会自动纳新——而这正是他需要一眼看到的信息。

## 7. 导入导出与回退

### 7.1 格式

`PortableRule` 加 `applyToNewInbounds bool`，**值类型而非指针**。

与 `PortableRule.InboundRefs` 的指针类型形成对照：那边必须区分「字段缺失」与「显式 `[]`」，因为 `[]` 在那里另有「对所有入站生效」这个特殊含义；这边「键缺失」与 `false` 都只意味着「不自动纳新」，是同义词，加指针只会多出一处需要解释的不对称。与 `PortableDomainGroup.Cidrs` 的取舍相同。

导入路径不新增校验：新约束由 `checkConflict` 自动继承。

### 7.2 兼容性

| 方向 | 结果 | 安全性 |
|---|---|---|
| 旧文件导入新面板 | 无此键 → `false` | 范围缩小 |
| 新文件导入旧面板 | 键被忽略 | 范围缩小 |
| 回退到旧二进制 | 整列被忽略，规则即 `inbound_ids` 的内容 | 范围缩小，与升级前一致 |

三个方向都落在安全侧。

## 8. 不做的事

**删除入站时不自动从规则里摘掉。** `CheckInboundRefs` 维持现状：有规则引用就拒绝删除，管理员必须先逐条摘掉。

代价是真实的：扩散会让每个入站被更多规则引用，此后删一个用户要先从更多规则里摘他。

不做的理由：删除入站是低频操作；让 `DelInbound` 按 `ApplyToNewInbounds` 分叉会多出一处能出错的地方；而 `CheckInboundRefs` 除了防 SQLite id 复用（删掉的 id 会被下一个新建的入站拿到，孤儿规则静默绑上去）之外，还承担着「让管理员知道这个人还在几条规则里」的作用。等它真的碍事了再单独设计。

## 9. 测试

`web/service/routing_rule_test.go` / `inbound_test.go`：

- 建入站后，声明了自动应用的规则包含它；未声明的规则不包含。
- **`InboundIds` 为空数组的规则被跳过**——建入站后它仍是 `[]`。这条守 §4.2 那个高危失误模式。
- 扩散写库失败时入站也不落库（事务回滚）。
- `AddInbounds`（v2ui 路径）同样扩散。
- 同一域名组下第二条声明自动应用的规则被 `checkConflict` 拒绝，错误文案含「冲突」。
- 一条规则引用多个域名组时逐组判定：只要任一组已有声明者即拒绝。
- 扩散后 `InboundIds` 仍是升序去重（生成确定性）。
- 导入导出往返保持该字段。

`web/html_test.go` 的 `TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot` 自动覆盖模板改动。

## 10. 改动文件清单

| 文件 | 改动 |
|---|---|
| `database/model/routing.go` | `RoutingRule` 加 `ApplyToNewInbounds` |
| `web/service/routing_rule.go` | 新增 `AttachInbound`；`checkConflict` 加 §5.1 判定；`Update` 同步该字段 |
| `web/service/inbound.go` | `AddInbound` / `AddInbounds` 改事务并调 `AttachInbound` |
| `web/service/routing_portable.go` | `PortableRule` 加字段，导出与导入各接一处 |
| `web/controller/routing.go` | `routingRuleForm` / `routingRuleView` 加字段 |
| `web/assets/js/model/routing.js` | `RoutingRule` 构造函数与 `fromJson` 各加一处 |
| `web/html/xui/routing.html` | 弹窗复选框与联动、规则列表标记 |
| `web/html/xui/component/routing_rule_table.html` | 「用户」列的 `+新用户` 标记 |
