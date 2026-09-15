package controller

import (
	"path/filepath"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// 前端按注入的 panelTimeZone 显示时间（web/assets/js/util/date-util.js），服务端
// 的曲线刻度与用量窗口按 SettingService.GetTimeLocation 算。两边必须是同一个
// 口径：库里存了非法值时服务端回落到默认时区，注入的名字也得跟着回落——原样
// 注入的话，浏览器会因为不认识这个名字而悄悄退回本地时区，表格与曲线再次错开。
func TestPanelTimeZoneFollowsServerLocation(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if got := panelTimeZone(); got != "Asia/Shanghai" {
		t.Fatalf("未设置时区时应注入默认值 Asia/Shanghai，得到 %q", got)
	}

	cases := []struct{ stored, want string }{
		{"America/New_York", "America/New_York"},
		{"Mars/Olympus", "Asia/Shanghai"},
	}
	for _, tc := range cases {
		setTimeLocation(t, tc.stored)
		if got := panelTimeZone(); got != tc.want {
			t.Errorf("库里存 %q 时应注入 %q，得到 %q", tc.stored, tc.want, got)
		}
	}
}

func setTimeLocation(t *testing.T, value string) {
	t.Helper()
	db := database.GetDB()
	if err := db.Where("key = ?", "timeLocation").Delete(&model.Setting{}).Error; err != nil {
		t.Fatalf("清掉旧的时区设置: %v", err)
	}
	if err := db.Create(&model.Setting{Key: "timeLocation", Value: value}).Error; err != nil {
		t.Fatalf("写入时区设置: %v", err)
	}
}
