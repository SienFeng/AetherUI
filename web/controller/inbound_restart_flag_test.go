package controller

import (
	"path/filepath"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// 重启标志是先 CAS 清掉、再调 RestartXray 的。下发失败（合成配置时库被锁、模板坏了、写配置
// 失败……）而不把标志放回去，这次改动就要等下一次别的改动才会再被尝试：核心一直跑着旧配置，
// 停用的入站照常能用，面板却显示已保存。
func TestApplyPendingXrayChangeRequeuesOnFailure(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	a := &InboundController{}
	a.xrayService.IsNeedRestartAndSetFalse() // 清掉别的用例留下的标志
	t.Cleanup(func() { a.xrayService.IsNeedRestartAndSetFalse() })

	// 模板不是合法 JSON：GetXrayConfig 必然失败，RestartXray 返回错误。
	if err := database.GetDB().Create(&model.Setting{Key: "xrayTemplateConfig", Value: "{not json"}).Error; err != nil {
		t.Fatal(err)
	}

	a.xrayService.SetToNeedRestart()
	a.applyPendingXrayChange()

	if !a.xrayService.IsNeedRestartAndSetFalse() {
		t.Fatal("下发失败后重启标志被消费掉了，这次改动不会再被重试")
	}
}
