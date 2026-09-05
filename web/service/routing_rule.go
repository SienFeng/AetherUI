package service

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

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
	groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil {
		return common.NewError("域名组数据损坏:", err)
	}
	// 空集合绝不放行：domain 条件为空会让规则劫持该用户的全部流量，
	// 而 xray 返回 Configuration OK，没有任何一层会报错。
	if len(groupIds) == 0 {
		return common.NewError("必须至少指定一个域名组")
	}
	for _, id := range groupIds {
		if _, err := s.domainGroupService.Get(id); err != nil {
			return common.NewError("域名组不存在:", id)
		}
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

// intersectInbounds 判断两个覆盖集合是否相交。空切片代表全集（所有用户）。
//
// 第二个返回值是相交的具体入站 id；任一边是全集时返回 0，此时错误信息该说
// 「所有用户」而不是点某个人的名。b 已升序，取到的是最小的相交 id，
// 保证错误信息稳定可测。
func intersectInbounds(a, b []int) (bool, int) {
	if len(a) == 0 || len(b) == 0 {
		return true, 0
	}
	set := make(map[int]bool, len(a))
	for _, id := range a {
		set[id] = true
	}
	for _, id := range b {
		if set[id] {
			return true, id
		}
	}
	return false, 0
}

// ruleLabel 给规则一个人能认出来的名字，备注为空时退回 id。
func ruleLabel(rule *model.RoutingRule) string {
	if rule.Remark != "" {
		return rule.Remark
	}
	return "规则 #" + strconv.Itoa(rule.Id)
}

// inboundLabel 把相交的入站说清楚。id 为 0 表示冲突来自「所有用户」那一侧，
// 此时没有具体的人可点名。
func inboundLabel(id int) string {
	if id <= 0 {
		return "「所有用户」"
	}
	in, err := (&InboundService{}).GetInbound(id)
	if err != nil || in == nil {
		return "入站 #" + strconv.Itoa(id)
	}
	if in.Remark != "" {
		return "用户「" + in.Remark + "」"
	}
	return "入站「" + in.Tag + "」"
}

// groupLabel 取域名组备注，取不到就退回 id——报错信息本身不该再失败。
func (s *RoutingRuleService) groupLabel(id int) string {
	group, err := s.domainGroupService.Get(id)
	if err != nil || group == nil {
		return "#" + strconv.Itoa(id)
	}
	return group.Remark
}

// checkConflict 保证「同一个域名组下，任何一个入站至多被一条规则覆盖」。
//
// 把每条规则看作它覆盖的入站集合：InboundIds 为空数组表示全集（含以后新建
// 的入站），否则就是那些 id 的集合。两条规则冲突，当且仅当**域名组集合相交**
// 且入站集合相交——全集与任何集合相交，也与另一个全集相交，「所有用户」与
// 「指定用户」的严格互斥就是这条判定的自然结果，不需要额外分支。
//
// 不过滤 Enable：禁用的规则同样占位，否则会出现「保存时没问题、一启用才
// 发现撞车」。想腾位置就得先改掉或删掉旧规则。
//
// 只在写入路径生效，绝不在生成期干预。迁移前写入的冲突数据照常生成两条
// 规则，行为与本功能上线前一致；生成期悄悄丢一条，等于在管理员不知情时
// 改变分流行为。历史冲突由界面标黄暴露，交给人决定改哪条。
func (s *RoutingRuleService) checkConflict(rule *model.RoutingRule) error {
	ids, err := DecodeInboundIds(rule.InboundIds)
	if err != nil {
		return common.NewError("入站数据损坏:", err)
	}
	groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil {
		return common.NewError("域名组数据损坏:", err)
	}

	// 域名组是 JSON 数组列，没法再用 WHERE domain_group_id = ? 交给 SQL 过滤，
	// 只能读出全部规则逐条解码。规则是几十条量级，这点开销换掉一张关联表是
	// 划算的——与 CheckInboundRefs 同一个取舍。
	others, err := s.GetAll()
	if err != nil {
		return err
	}

	for _, other := range others {
		if other.Id == rule.Id {
			continue
		}
		otherGroupIds, decodeErr := DecodeDomainGroupIds(other.DomainGroupIds)
		if decodeErr != nil {
			// 无从判断它覆盖了哪些组。不拦——它自己已经会被 buildRule 整条
			// 丢弃，再拿它去挡别人只会让管理员既修不了旧规则也建不了新规则。
			continue
		}
		sharedGroup, whichGroup := intersectGroups(groupIds, otherGroupIds)
		if !sharedGroup {
			continue
		}
		otherIds, decodeErr := DecodeInboundIds(other.InboundIds)
		if decodeErr != nil {
			continue
		}
		overlap, who := intersectInbounds(ids, otherIds)
		if !overlap {
			continue
		}
		// 用 NewErrorf 而不是 NewError：后者走 fmt.Sprintln，会在每个参数
		// 之间插空格，拼出「与分流规则「 甲 」冲突」这种带空隙的句子。
		//
		// 注意：routing_portable.go 的 importRules 用
		// strings.Contains(err.Error(), "冲突") 把这类错误识别成「本机已存在
		// 同覆盖范围的规则」（计入 Skipped 而不是 Failed，导入才能保持幂等）。
		// 改这句文案（尤其是去掉「冲突」二字）前，先去同步看那一处。
		return common.NewErrorf(
			"与分流规则「%s」冲突：%s在域名组「%s」下已被它覆盖。同一个用户在同一个域名组下只能有一条规则。",
			ruleLabel(other), inboundLabel(who), s.groupLabel(whichGroup))
	}
	return nil
}

func (s *RoutingRuleService) Add(rule *model.RoutingRule) error {
	if err := s.validate(rule); err != nil {
		return err
	}
	if err := s.checkConflict(rule); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Save(rule).Error
}

func (s *RoutingRuleService) Update(rule *model.RoutingRule) error {
	if err := s.validate(rule); err != nil {
		return err
	}
	if err := s.checkConflict(rule); err != nil {
		return err
	}
	old, err := s.Get(rule.Id)
	if err != nil {
		return err
	}
	old.Remark = rule.Remark
	old.InboundIds = rule.InboundIds
	old.DomainGroupIds = rule.DomainGroupIds
	// 过渡桥接：buildRule（routing_inject.go）、listRules（controller/routing.go）、
	// toPortableRule（routing_portable.go）三个消费者眼下仍只读 DomainGroupId，
	// 而 Update 不会再有任何后续动作去重新同步它——编辑一条规则的域名组会让
	// 这三处永远停在改动前的值，且不报任何错。用解码后 DomainGroupIds 的首个
	// id 同步 DomainGroupId，保持这三个消费者在过渡期内跟上编辑；Task 9 收尾时
	// 三个消费者切到 DomainGroupIds 后删除这行。
	//
	// 解码失败或为空时保持原值不动：validate 已经在上面拒绝过这两种情况，
	// 这里走不到，但不能让这行代码自己成为一个 panic 点。
	if groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds); err == nil && len(groupIds) > 0 {
		old.DomainGroupId = groupIds[0]
	}
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

// CheckDomainGroupRefs 在删除域名组前调用。
//
// 域名组一旦消失，引用它的规则会少掉这一组的域名；若它是规则引用的唯一
// 一组，合并结果为空，buildRule 会整条丢弃——本该走指定节点或被封禁的
// 流量静默退回直连。
//
// 更危险的是 SQLite 会复用自增主键 id：删掉 Claude 组再新建 ChatGPT 组
// 可能拿到同一个 id，孤儿规则会静默变成「ChatGPT 的域名走 Claude 的节点」。
// 那时引用不再悬空，生成期的跳过防线拦不住，规则列表还会渲染得完全正常。
//
// DomainGroupIds 是 JSON 数组列，没法交给 SQL 去数，只能读出来逐条解码，
// 与 CheckInboundRefs 同形。
func (s *RoutingRuleService) CheckDomainGroupRefs(groupId int) error {
	if groupId <= 0 {
		return nil
	}
	rules, err := s.GetAll()
	if err != nil {
		return err
	}
	count := 0
	for _, rule := range rules {
		ids, decodeErr := DecodeDomainGroupIds(rule.DomainGroupIds)
		if decodeErr != nil {
			// 数据损坏时无从判断这条规则引用了哪些组。宁可拦住删除：放行
			// 的话，SQLite 复用 id 后这条规则可能静默绑到新建的域名组上。
			return common.NewError("分流规则", rule.Id,
				"的域名组数据已损坏，无法确认引用关系，请先修复或删除该规则")
		}
		for _, id := range ids {
			if id == groupId {
				count++
				break
			}
		}
	}
	if count > 0 {
		return common.NewError("该域名组仍被", count, "条分流规则引用，请先删除这些规则")
	}
	return nil
}

// CheckInboundRefs 在删除入站前调用。
//
// SQLite 的自增主键 id 会被复用：GORM 的 sqlite 驱动对 primaryKey;autoIncrement
// 生成的是 rowid 别名而非 AUTOINCREMENT，删掉最大 id 的行后，新插入的行会拿到
// 同一个 id。删掉用户甲的入站再新建用户丙的入站，「甲的 ChatGPT 走 B 节点」这条
// 孤儿规则会静默重绑到丙身上，而规则列表还会渲染得很合理。
//
// 生成期跳过那道防线拦不住这种情况——引用不再悬空，只是指错了人。
//
// InboundIds 为空数组是「所有用户」规则，不指向任何具体入站，不参与本检查。
func (s *RoutingRuleService) CheckInboundRefs(inboundId int) error {
	if inboundId <= 0 {
		return nil
	}
	// InboundIds 是 JSON 数组列，没法交给 SQL 去数，只能读出来逐条解码。
	// 规则是几十条量级，这点开销换掉一张关联表是划算的。
	rules, err := s.GetAll()
	if err != nil {
		return err
	}
	count := 0
	for _, rule := range rules {
		ids, decodeErr := DecodeInboundIds(rule.InboundIds)
		if decodeErr != nil {
			// 数据损坏时无从判断这条规则引用了谁。宁可拦住删除：放行的话，
			// SQLite 复用 id 后这条规则可能静默绑到新建的入站上。
			return common.NewError("分流规则", rule.Id,
				"的入站数据已损坏，无法确认引用关系，请先修复或删除该规则")
		}
		for _, id := range ids {
			if id == inboundId {
				count++
				break
			}
		}
	}
	if count > 0 {
		return common.NewError("该入站仍被", count, "条分流规则引用，请先删除这些规则")
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

// EncodeInboundIds 把入站 id 列表编成入库格式：丢弃非正数、去重、升序。
//
// 升序是「生成逐字节确定」不变量的一部分，见 model.RoutingRule.InboundIds 的注释。
//
// 注意本函数会丢弃非正数，因此 [0] 这类输入会得到 "[]"——而 "[]" 的语义是
// 「所有用户」。写入路径一律用 EncodeInboundIdsStrict，不要直接用这个。
func EncodeInboundIds(ids []int) (string, error) {
	seen := make(map[int]bool, len(ids))
	cleaned := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		cleaned = append(cleaned, id)
	}
	sort.Ints(cleaned)
	b, err := json.Marshal(cleaned)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// EncodeInboundIdsStrict 是写入路径该用的版本：原始列表非空、却没有任何
// 有效 id 时报错，而不是降级成「所有用户」。
//
// 一次前端 bug 或手工构造的请求，就能把一条本该只覆盖某个人的规则悄悄
// 放大到全体，且一路合法——「所有用户」只能来自前端那个显式的复选框，
// 也就是原始列表本来就是空的情况。
func EncodeInboundIdsStrict(ids []int) (string, error) {
	encoded, err := EncodeInboundIds(ids)
	if err != nil {
		return "", err
	}
	if len(ids) > 0 && encoded == "[]" {
		return "", common.NewError("入站选择非法：提交了", len(ids), "个入站，但没有一个是有效的入站 id")
	}
	return encoded, nil
}

// DecodeInboundIds 是 EncodeInboundIds 的逆操作。
//
// 空字符串与 "null" 当作空数组（= 所有用户）：迁移会回填，但直接改库、
// 并发写入等路径仍可能留下空值，在这里报错会让整份配置生成失败。
// 真正的语法错误仍返回 error，交给调用方整条丢弃该规则。
func DecodeInboundIds(encoded string) ([]int, error) {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var ids []int
	if err := json.Unmarshal([]byte(trimmed), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// EncodeDomainGroupIds 把域名组 id 列表编成入库格式：丢弃非正数、去重、升序。
//
// 升序去重是「生成逐字节确定」的一部分：buildRule 按这个顺序逐组取域名再
// 合并，顺序一抖动，Config.Equals 恒为 false，那个 10 秒的重启 cron 会不停
// 重启 xray。
//
// 注意本函数会丢弃非正数，因此 [0] 这类输入会得到 "[]"——而空的域名组集合
// 会让规则的 domain 条件为空。写入路径一律用 EncodeDomainGroupIdsStrict。
func EncodeDomainGroupIds(ids []int) (string, error) {
	seen := make(map[int]bool, len(ids))
	cleaned := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		cleaned = append(cleaned, id)
	}
	sort.Ints(cleaned)
	b, err := json.Marshal(cleaned)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// EncodeDomainGroupIdsStrict 是写入路径该用的版本。
//
// 与 EncodeInboundIdsStrict 的关键分歧：那边只在「原始非空却编出 []」时报错，
// 因为入站的空列表是用户通过「所有用户」复选框显式表达的合法语义。域名组
// 没有对应的概念——空的域名组集合意味着 domain 条件为空，xray 会把它当作
// 「不限制」，规则从「这批域名走 B」退化成「该用户全部流量走 B」，且返回
// Configuration OK、面板首页显示 running。所以这里对空结果一律报错，
// 无论原始列表是否为空。
func EncodeDomainGroupIdsStrict(ids []int) (string, error) {
	encoded, err := EncodeDomainGroupIds(ids)
	if err != nil {
		return "", err
	}
	if encoded == "[]" {
		if len(ids) == 0 {
			return "", common.NewError("必须至少指定一个域名组")
		}
		return "", common.NewError("域名组选择非法：提交了", len(ids),
			"个域名组，但没有一个是有效的域名组 id")
	}
	return encoded, nil
}

// DecodeDomainGroupIds 是 EncodeDomainGroupIds 的逆操作。
//
// 空字符串与 "null" 当作空切片且不报错：迁移会回填，但直接改库、并发写入
// 等路径仍可能留下空值，在这里报错会让整份配置生成失败。空切片本身是非法
// 状态，由 validate（拒绝写入）与 buildRule（整条丢弃）各自处理。
// 真正的语法错误仍返回 error，交给调用方整条丢弃该规则。
func DecodeDomainGroupIds(encoded string) ([]int, error) {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var ids []int
	if err := json.Unmarshal([]byte(trimmed), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// intersectGroups 判断两个域名组集合是否相交。
//
// 与 intersectInbounds 的关键分歧：那边把空切片当全集（「所有用户」），
// 这里的空集合是非法值而不是全集，绝不能复用——复用会让两条各自损坏的
// 规则被判成互相冲突，管理员既修不了旧规则也建不了新规则。
//
// 第二个返回值是相交的最小 id（b 已升序），保证错误信息稳定可测。
func intersectGroups(a, b []int) (bool, int) {
	if len(a) == 0 || len(b) == 0 {
		return false, 0
	}
	set := make(map[int]bool, len(a))
	for _, id := range a {
		set[id] = true
	}
	for _, id := range b {
		if set[id] {
			return true, id
		}
	}
	return false, 0
}
