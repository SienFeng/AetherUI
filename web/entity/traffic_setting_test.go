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
		WebPort:                  54321,
		WebBasePath:              "/",
		TimeLocation:             "Asia/Shanghai",
		XrayTemplateConfig:       "{}",
		SubscriptionUpdateTime:   "04:00",
		IPDBSourceUrl:            "https://example.com/ipv4_source.txt",
		QQWrySourceUrl:           "https://example.com/qqwry.dat",
		IPDBUpdateTime:           "",
		AccessLogEnable:          0,
		AccessLogRetentionDays:   7,
		TCInterface:              "",
		TrafficHourRetentionDays: 30,
		TrafficDayRetentionDays:  365,
	}
}
