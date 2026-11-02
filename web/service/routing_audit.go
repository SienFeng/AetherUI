package service

import (
	"errors"
	"sync"
	"time"

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
// AnomalyCount/AnomalyLastAt 来自访问日志持续扫描（routing_anomaly.go），是最近 24 小时
// 走错的真实连接数与最近一次的时刻；有异常时 Verdict 一律是 mismatch。
// Stale 表示缓存结果的代次落后于当前代次（核心之后又换过配置，或本轮预算用完没轮到），
// 此时 Verdict 一律是 pending：旧结论不代表核心现状，不能给绿。
type RuleAuditSummary struct {
	Verdict       string `json:"verdict"`
	CheckedAt     int64  `json:"checkedAt"`
	ApplyGen      uint64 `json:"applyGen"`
	Stale         bool   `json:"stale"`
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
// 没有用户行时没有任何证据：核心 pending → pending，其余（含 ok）→ unverifiable，不给绿。
func ruleVerdictOf(res *RuleVerifyResult) (verdict string, mismatchUsers int) {
	if res == nil {
		return verdictUnverifiable, 0
	}
	if len(res.Users) == 0 {
		if res.Core.State == coreStatePending {
			return verdictPending, 0
		}
		return verdictUnverifiable, 0
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

// RoutingAuditService 是后台审计的入口：跑一轮全量验证写缓存、扫描访问日志异常，
// 以及读出规则级摘要。零值可用；verify 为 nil 时用默认的 RoutingVerifyService，
// now 为 nil 时用 time.Now，测试可注入。
type RoutingAuditService struct {
	verify *RoutingVerifyService
	now    func() time.Time
	// runtime 是 Summaries 判断「有没有待生效改动」用的核心状态来源；nil 时取真实快照。
	runtime func() XrayRuntime
}

func (s *RoutingAuditService) runtimeSnapshot() XrayRuntime {
	if s.runtime != nil {
		return s.runtime()
	}
	// 待生效判定走 5 秒记忆：每个开着的页面每 60 秒轮询一次，不必每次都合成配置。
	return (&XrayService{}).runtimeSnapshot(summaryPendingMemo)
}

func (s *RoutingAuditService) nowTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *RoutingAuditService) verifier() *RoutingVerifyService {
	if s.verify != nil {
		return s.verify
	}
	return &RoutingVerifyService{}
}

// RunAudit 跑一轮全量验证并写缓存，返回本轮使用的应用代次。
// 错误原样返回；ErrVerifyBusy / ErrCoreNotReady 时缓存一个字节都不动。
// ErrVerifyTruncated（预算用完）时已跑完的规则照常写入，但仍返回该错误：
// 这一轮不完整，调用方不能把它记成一次成功的审计。
func (s *RoutingAuditService) RunAudit() (uint64, error) {
	results, gen, err := s.verifier().VerifyAll()
	if err != nil && !errors.Is(err, ErrVerifyTruncated) {
		return 0, err
	}
	partial := err
	rules, err := (&RoutingRuleService{}).GetEnabled()
	if err != nil {
		// 结果本身有效，照常写入；只是这一轮没法判断哪些规则已删除/停用，留到下一轮清理。
		logger.Warning("路由审计：读取启用规则失败，本轮不清理已删除的规则：", err)
		for _, res := range results {
			storeVerifyResult(res)
		}
		return gen, partial
	}
	ids := make([]int, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.Id)
	}
	replaceAuditResults(results, ids)
	return gen, partial
}

// Summaries 返回当前应用代次与每条启用规则的摘要。没有缓存的规则 verdict=unverifiable、
// CheckedAt=0；缓存代次 ≠ 当前代次的标 Stale、verdict=pending（MismatchUsers 不再沿用旧值）。
// 读启用规则失败时记日志并返回空表（接口签名不带 error）。
//
// 并入访问日志异常：有异常即 mismatch，不论核心结果是否 stale、有没有缓存。异常是真实
// 流量的证据，与代次无关——规则改过之后，改之前的走错仍在 24 小时窗口内，列表照样
// 显示不符并给出最近一次的时刻（设计 §4.4），管理员确认修复后用「忽略此前的异常」清掉。
// Stale/MismatchUsers 仍只描述核心验证那一路，只有 Verdict 被异常覆盖。
//
// 待生效窗口：保存后到核心载入新配置之前（最多约 10 秒）代次还没变，同代次的缓存
// 却是旧配置的结论。此时（快照 Pending，或 Busy——重启/热应用正占着锁、配置正在换）
// 本会是 verified 的摘要一律改标 Stale + pending，不给绿。
// 核心未运行时同理不给绿：缓存是它停下之前的结论，而轮询照常成功、前端的过时判断
// 不会生效，下一轮审计又要等最多一个周期——本会是 verified 的改标 Stale + unverifiable。
// 两种情形下不符与无法验证都原样保留，访问日志异常仍覆盖为不符。
// 快照要合成一次配置：整轮最多取一次，且只在确有 verified 摘要时才取。
func (s *RoutingAuditService) Summaries() (applyGen uint64, rules map[int]RuleAuditSummary) {
	applyGen = XrayApplyGeneration()
	now := s.nowTime()
	var rt XrayRuntime
	rtLoaded := false
	// notCurrent 返回同代次缓存里的 verified 此刻应降成的结论；空串表示仍可给绿。
	// Busy 时快照的 Running 恒为 false，必须先判 Busy，否则会误报成「未运行」。
	// 快照的代次 ≠ 开头读到的 applyGen：两次读取之间核心换过配置（热应用/重启），
	// 与 applyGen 相等的缓存是换之前的结论，同样按 pending。
	notCurrent := func() string {
		if !rtLoaded {
			rt, rtLoaded = s.runtimeSnapshot(), true
		}
		switch {
		case rt.Busy || rt.Pending || rt.ApplyGen != applyGen:
			return verdictPending
		case !rt.Running:
			return verdictUnverifiable
		}
		return ""
	}
	rules = map[int]RuleAuditSummary{}
	enabled, err := (&RoutingRuleService{}).GetEnabled()
	if err != nil {
		logger.Warning("路由审计：读取启用规则失败：", err)
		return applyGen, rules
	}
	for _, r := range enabled {
		sum := RuleAuditSummary{Verdict: verdictUnverifiable}
		if res, ok := cachedVerifyResult(r.Id); ok {
			sum.CheckedAt, sum.ApplyGen = res.CheckedAt, res.ApplyGen
			if res.ApplyGen != applyGen {
				sum.Stale, sum.Verdict = true, verdictPending
			} else {
				sum.Verdict, sum.MismatchUsers = ruleVerdictOf(res)
				if sum.Verdict == verdictVerified {
					if v := notCurrent(); v != "" {
						sum.Stale, sum.Verdict = true, v
					}
				}
			}
		}
		sum.AnomalyCount, sum.AnomalyLastAt = anomalyTotals(r.Id, now)
		if sum.AnomalyCount > 0 {
			sum.Verdict = verdictMismatch
		}
		rules[r.Id] = sum
	}
	return applyGen, rules
}

// Cached 返回一条规则的缓存结果（只读，见包级缓存的说明）。
func (s *RoutingAuditService) Cached(ruleId int) (*RuleVerifyResult, bool) {
	return cachedVerifyResult(ruleId)
}
