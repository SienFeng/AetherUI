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
