package service

import (
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// getSettingValue 直接读 settings 表里某个 key 当前的值，不经过
// SettingService.getString 的 defaultValueMap 回落——这里就是要确认「库里
// 的值」变没变，不是「读出来的最终值」（那个哪怕库里没有行，也会回落到
// 默认值，看不出有没有写库）。
func getSettingValue(t *testing.T, key string) (string, bool) {
	t.Helper()
	db := database.GetDB()
	setting := &model.Setting{}
	err := db.Model(model.Setting{}).Where("key = ?", key).First(setting).Error
	if database.IsNotFound(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("查询 setting[%s] 失败: %v", key, err)
	}
	return setting.Value, true
}

// TestGetXrayConfigTemplateMigratesAndPersists 覆盖 C4：GetXrayConfigTemplate
// 是唯一一段「每台存量机器升级后第一次启动就会执行」的迁移路径，而且它让
// 一个看起来是纯读的函数带上了写库的副作用（读一次补一次 RoutingService，
// 补完立刻落库）。修复前只有纯函数 ensureRoutingServiceInTemplate 被测过，
// 这条读→补→写回的环完全没有测试覆盖。
//
// 断言三条：
//  1. 库里存的旧模板不含 RoutingService，第一次调用后库里的值已经含
//     RoutingService（不只是返回值）。
//  2. 连续调用两次，第二次返回值与第一次逐字节相同，且库里的值在两次调用
//     之间没有再变化——这条守的是「迁移一次后稳定」，不稳定的话那个 10 秒
//     的重启 cron（Config.Equals 逐字节比较）会不停重启 xray。
func TestGetXrayConfigTemplateMigratesAndPersists(t *testing.T) {
	setupDB(t)
	s := &SettingService{}

	oldTemplate := `{"api":{"services":["HandlerService"],"tag":"api"}}`
	if err := s.setString("xrayTemplateConfig", oldTemplate); err != nil {
		t.Fatalf("写入旧模板失败: %v", err)
	}

	first, err := s.GetXrayConfigTemplate()
	if err != nil {
		t.Fatalf("第一次调用失败: %v", err)
	}
	if !strings.Contains(first, "RoutingService") {
		t.Fatalf("第一次调用的返回值未含 RoutingService，实际: %s", first)
	}

	storedAfterFirst, ok := getSettingValue(t, "xrayTemplateConfig")
	if !ok {
		t.Fatal("第一次调用后库里应已存在 xrayTemplateConfig")
	}
	if !strings.Contains(storedAfterFirst, "RoutingService") {
		t.Fatalf("第一次调用后库里的值未含 RoutingService，实际: %s", storedAfterFirst)
	}
	if storedAfterFirst != first {
		t.Fatalf("库里的值应与第一次返回值逐字节相同\n库: %s\n返回: %s", storedAfterFirst, first)
	}

	second, err := s.GetXrayConfigTemplate()
	if err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	if second != first {
		t.Fatalf("第二次调用应返回逐字节相同的结果\n第一次: %s\n第二次: %s", first, second)
	}

	storedAfterSecond, ok := getSettingValue(t, "xrayTemplateConfig")
	if !ok {
		t.Fatal("第二次调用后库里应仍存在 xrayTemplateConfig")
	}
	if storedAfterSecond != storedAfterFirst {
		t.Fatalf("第二次调用不应再改动库里的值\n第一次后: %s\n第二次后: %s", storedAfterFirst, storedAfterSecond)
	}
}

// TestGetXrayConfigTemplateFailsOpenOnInvalidJSON 覆盖 C4 的第三条断言：
// 模板本身就不是合法 JSON 时（既有问题，不是这条迁移路径造成的），
// GetXrayConfigTemplate 必须 fail open——原样返回原串、error 为 nil，
// 而不是把管理员锁在门外连设置页都打不开。
func TestGetXrayConfigTemplateFailsOpenOnInvalidJSON(t *testing.T) {
	setupDB(t)
	s := &SettingService{}

	invalid := `{"api":`
	if err := s.setString("xrayTemplateConfig", invalid); err != nil {
		t.Fatalf("写入非法模板失败: %v", err)
	}

	got, err := s.GetXrayConfigTemplate()
	if err != nil {
		t.Fatalf("非法 JSON 时应 fail open 返回 nil error，实际返回了 error: %v", err)
	}
	if got != invalid {
		t.Fatalf("非法 JSON 时应原样返回原串，实际: %q，期望: %q", got, invalid)
	}
}
