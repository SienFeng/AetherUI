package service

import (
	"sync"

	"a-ui/logger"
)

// 路由审计的结果缓存：每条规则只留最新一份验证结果，只在内存里（审计设计 §3.4）。
// 面板重启必然重启 xray、代次一变，下一轮审计就会重算，持久化收益很低；
// 落库还会引入带 rule_id 外键的表与 id 复用问题。
//
// 缓存里的结果视为只读：存进来之后任何一方都不得再修改它。
var (
	auditMu      sync.Mutex
	auditResults = map[int]*RuleVerifyResult{}
)

// storeVerifyResult 写入一条结果：应用代次新者胜；同代次按 CheckedAt 新者胜，
// 相等时新来的胜（同一时刻的两份结果没有先后可言，取后到的）。
func storeVerifyResult(res *RuleVerifyResult) {
	if res == nil {
		return
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	storeVerifyResultLocked(res)
}

func storeVerifyResultLocked(res *RuleVerifyResult) {
	if old, ok := auditResults[res.RuleId]; ok {
		if old.ApplyGen > res.ApplyGen || (old.ApplyGen == res.ApplyGen && old.CheckedAt > res.CheckedAt) {
			return
		}
	}
	auditResults[res.RuleId] = res
}

// replaceAuditResults 写入一轮审计的结果，并移除不在 enabledIds 里的规则（已删除或停用）。
// 写入同样走新者胜：一轮审计结束到写缓存之间可能插进一次更新的手动验证。
// 启用但本轮没有结果的规则（预算用完没轮到）保留旧结果；不在 enabledIds 里的结果不写入。
func replaceAuditResults(results map[int]*RuleVerifyResult, enabledIds []int) {
	enabled := make(map[int]bool, len(enabledIds))
	for _, id := range enabledIds {
		enabled[id] = true
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	for id := range auditResults {
		if !enabled[id] {
			delete(auditResults, id)
		}
	}
	for id, res := range results {
		if res != nil && enabled[id] {
			storeVerifyResultLocked(res)
		}
	}
}

// cachedVerifyResult 返回一条规则的缓存结果。
func cachedVerifyResult(ruleId int) (*RuleVerifyResult, bool) {
	auditMu.Lock()
	defer auditMu.Unlock()
	res, ok := auditResults[ruleId]
	return res, ok
}

// RuleAuditSummary 是规则列表与侧栏红点用的规则级状态。
// AnomalyCount/AnomalyLastAt 来自访问日志持续扫描，由后续任务填充，目前恒为 0。
type RuleAuditSummary struct {
	Verdict       string `json:"verdict"`
	CheckedAt     int64  `json:"checkedAt"`
	ApplyGen      uint64 `json:"applyGen"`
	MismatchUsers int    `json:"mismatchUsers"`
	AnomalyCount  int64  `json:"anomalyCount"`
	AnomalyLastAt int64  `json:"anomalyLastAt"`
}

// verdictRank 是用户结论的严重程度：不符 > 等待下发 > 无法验证 > 已验证，
// 与流量走向图的汇总口径一致。认不出的结论按「无法验证」处理，不给绿。
func verdictRank(v string) int {
	switch v {
	case verdictVerified:
		return 0
	case verdictPending:
		return 2
	case verdictMismatch:
		return 3
	default:
		return 1
	}
}

// verdictByRank 是 verdictRank 的反查。
var verdictByRank = [...]string{verdictVerified, verdictUnverifiable, verdictPending, verdictMismatch}

// ruleVerdictOf 把一条结果汇总成规则级结论：取用户里最坏的一档，并数出不符的用户数。
// 没有用户行时按核心级状态映射：ok → verified、pending → pending，其余 → unverifiable。
func ruleVerdictOf(res *RuleVerifyResult) (verdict string, mismatchUsers int) {
	if res == nil {
		return verdictUnverifiable, 0
	}
	if len(res.Users) == 0 {
		switch res.Core.State {
		case coreStateOK:
			return verdictVerified, 0
		case coreStatePending:
			return verdictPending, 0
		default:
			return verdictUnverifiable, 0
		}
	}
	worst := 0
	for _, u := range res.Users {
		if u.Verdict == verdictMismatch {
			mismatchUsers++
		}
		if r := verdictRank(u.Verdict); r > worst {
			worst = r
		}
	}
	return verdictByRank[worst], mismatchUsers
}

// RoutingAuditService 是后台审计的入口：跑一轮全量验证写缓存，以及读出规则级摘要。
// 零值可用；verify 为 nil 时用默认的 RoutingVerifyService，测试可注入伪造的。
type RoutingAuditService struct {
	verify *RoutingVerifyService
}

func (s *RoutingAuditService) verifier() *RoutingVerifyService {
	if s.verify != nil {
		return s.verify
	}
	return &RoutingVerifyService{}
}

// RunAudit 跑一轮全量验证并写缓存，返回本轮使用的应用代次。
// 错误原样返回；ErrVerifyBusy / ErrCoreNotReady 时缓存一个字节都不动。
func (s *RoutingAuditService) RunAudit() (uint64, error) {
	results, gen, err := s.verifier().VerifyAll()
	if err != nil {
		return 0, err
	}
	rules, err := (&RoutingRuleService{}).GetEnabled()
	if err != nil {
		// 结果本身有效，照常写入；只是这一轮没法判断哪些规则已删除/停用，留到下一轮清理。
		logger.Warning("路由审计：读取启用规则失败，本轮不清理已删除的规则：", err)
		for _, res := range results {
			storeVerifyResult(res)
		}
		return gen, nil
	}
	ids := make([]int, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.Id)
	}
	replaceAuditResults(results, ids)
	return gen, nil
}

// Summaries 返回当前应用代次与每条启用规则的摘要。没有缓存的规则 verdict=unverifiable、
// CheckedAt=0。读启用规则失败时记日志并返回空表（接口签名不带 error）。
func (s *RoutingAuditService) Summaries() (applyGen uint64, rules map[int]RuleAuditSummary) {
	applyGen = XrayApplyGeneration()
	rules = map[int]RuleAuditSummary{}
	enabled, err := (&RoutingRuleService{}).GetEnabled()
	if err != nil {
		logger.Warning("路由审计：读取启用规则失败：", err)
		return applyGen, rules
	}
	for _, r := range enabled {
		res, ok := cachedVerifyResult(r.Id)
		if !ok {
			rules[r.Id] = RuleAuditSummary{Verdict: verdictUnverifiable}
			continue
		}
		verdict, mismatch := ruleVerdictOf(res)
		rules[r.Id] = RuleAuditSummary{
			Verdict:       verdict,
			CheckedAt:     res.CheckedAt,
			ApplyGen:      res.ApplyGen,
			MismatchUsers: mismatch,
		}
	}
	return applyGen, rules
}

// Cached 返回一条规则的缓存结果（只读，见包级缓存的说明）。
func (s *RoutingAuditService) Cached(ruleId int) (*RuleVerifyResult, bool) {
	return cachedVerifyResult(ruleId)
}
