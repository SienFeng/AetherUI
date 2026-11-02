package service

// StoreVerifyResultForTest 供其他包（controller）的测试预置审计缓存：
// storeVerifyResult 不导出，而跨包测试没有别的办法造出一份缓存。
// 生产代码不得调用。
func StoreVerifyResultForTest(res *RuleVerifyResult) {
	storeVerifyResult(res)
}

// ResetAuditResultsForTest 清空审计缓存，避免测试间（规则 id 会随新库重复）互相污染。
// 生产代码不得调用。
func ResetAuditResultsForTest() {
	auditMu.Lock()
	defer auditMu.Unlock()
	auditResults = map[int]*RuleVerifyResult{}
}

// SeedAnomalyForTest 给一条规则记一次走错的连接（时刻 lastAt，毫秒），供 controller 的测试
// 造出「异常比缓存结果更新」的状态。生产代码不得调用。
func SeedAnomalyForTest(ruleId, inboundId int, lastAt int64) {
	anomalyMu.Lock()
	defer anomalyMu.Unlock()
	b := newAnomalyBucket()
	b.add(inboundId, "a-ui-default", 1, lastAt)
	anomalySummaries[ruleId] = map[int64]*anomalyBucket{lastAt / anomalyBucketMs: b}
}

// ResetAnomaliesForTest 清空异常摘要与忽略时刻。生产代码不得调用。
func ResetAnomaliesForTest() {
	anomalyMu.Lock()
	defer anomalyMu.Unlock()
	anomalySummaries = map[int]map[int64]*anomalyBucket{}
	anomalyIgnore = map[int]int64{}
}
