package entity

import "testing"

func TestCheckValidRejectsOutOfRangeTrafficRetention(t *testing.T) {
	cases := []struct {
		name  string
		hour  int
		day   int
		valid bool
	}{
		{"默认值", 30, 365, true},
		{"下界", 1, 1, true},
		{"上界", 365, 3650, true},
		{"小时桶为 0", 0, 365, false},
		{"小时桶超上界", 366, 365, false},
		{"日桶为 0", 30, 0, false},
		{"日桶超上界", 30, 3651, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validBaseSettingForEntityTest()
			s.TrafficHourRetentionDays = c.hour
			s.TrafficDayRetentionDays = c.day
			err := s.CheckValid()
			if c.valid && err != nil {
				t.Errorf("期望通过，实际报错: %v", err)
			}
			if !c.valid && err == nil {
				t.Error("期望被拒绝，实际通过了")
			}
		})
	}
}

// validBaseSettingForEntityTest 与 service 包的 validBaseSetting 同一用意：
// CheckValid 是逐条串行校验的，前面任何一项不合法都会让后面的项根本走不到。
func validBaseSettingForEntityTest() *AllSetting {
	return &AllSetting{
		WebPort:                    54321,
		WebBasePath:                "/",
		TimeLocation:               "Asia/Shanghai",
		XrayTemplateConfig:         "{}",
		IPDBSourceUrl:              "https://example.com/ipv4_source.txt",
		QQWrySourceUrl:             "https://example.com/qqwry.dat",
		IPDBUpdateTime:             "",
		AccessLogEnable:            0,
		AccessLogRetentionDays:     7,
		TCInterface:                "",
		TrafficHourRetentionDays:   30,
		TrafficDayRetentionDays:    365,
		AutoTravelHomeSilenceHours: 2,
		AutoTravelStableDays:       3,
		AutoTravelPassHours:        72,
		AutoTravelMonthlyDays:      10,
		AutoTravelWeeklyGrants:     3,
		AutoTravelCooldownDays:     7,
	}
}

func TestCheckValidAutoTravelRanges(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*AllSetting)
		valid bool
	}{
		{"默认值", func(*AllSetting) {}, true},
		{"静默 0 小时", func(s *AllSetting) { s.AutoTravelHomeSilenceHours = 0 }, false},
		{"静默 25 小时", func(s *AllSetting) { s.AutoTravelHomeSilenceHours = 25 }, false},
		{"稳定 8 天（窗口只有 7 天）", func(s *AllSetting) { s.AutoTravelStableDays = 8 }, false},
		{"通行证 721 小时", func(s *AllSetting) { s.AutoTravelPassHours = 721 }, false},
		{"每月 0 天", func(s *AllSetting) { s.AutoTravelMonthlyDays = 0 }, false},
		{"每月 31 天", func(s *AllSetting) { s.AutoTravelMonthlyDays = 31 }, true},
		{"每周 0 次", func(s *AllSetting) { s.AutoTravelWeeklyGrants = 0 }, false},
		{"冷却 0 天（关闭）", func(s *AllSetting) { s.AutoTravelCooldownDays = 0 }, true},
		{"冷却 31 天", func(s *AllSetting) { s.AutoTravelCooldownDays = 31 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validBaseSettingForEntityTest()
			c.set(s)
			err := s.CheckValid()
			if c.valid && err != nil {
				t.Errorf("期望通过，实际报错: %v", err)
			}
			if !c.valid && err == nil {
				t.Error("期望被拒绝，实际通过了")
			}
		})
	}
}
