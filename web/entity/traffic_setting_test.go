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
		WebPort:                     54321,
		WebBasePath:                 "/",
		TimeLocation:                "Asia/Shanghai",
		XrayTemplateConfig:          "{}",
		IPDBSourceUrl:               "https://example.com/ipv4_source.txt",
		QQWrySourceUrl:              "https://example.com/qqwry.dat",
		IPDBUpdateTime:              "",
		AccessLogEnable:             0,
		AccessLogRetentionDays:      7,
		TCInterface:                 "",
		TrafficHourRetentionDays:    30,
		TrafficDayRetentionDays:     365,
		AutoTravelHomeSilenceHours:  2,
		AutoTravelStableDays:        3,
		AutoTravelPassHours:         72,
		AutoTravelMonthlyDays:       10,
		AutoTravelWeeklyGrants:      3,
		AutoTravelCooldownDays:      7,
		AutoTravelRelocateDays:      5,
		AutoRegionEnable:            0,
		AutoRegionObserveDays:       1,
		AutoRegionMaxBlockedPercent: 5,
		BlockIdcSources:             1,
		BlockSmtp:                   1,
		ForceSniffing:               1,
		SpikeThrottleEnable:         1,
		SpikeThrottleMultiplier:     10,
		SpikeThrottleFloorGB:        20,
		SpikeThrottleMbps:           5,
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
		{"长住改地区 0 天（关闭）", func(s *AllSetting) { s.AutoTravelRelocateDays = 0 }, true},
		{"长住改地区 7 天", func(s *AllSetting) { s.AutoTravelRelocateDays = 7 }, true},
		{"长住改地区 8 天（窗口只有 7 天）", func(s *AllSetting) { s.AutoTravelRelocateDays = 8 }, false},
		{"总开关开着时长住天数超过每月放行天数（通行证撑不到那天）", func(s *AllSetting) {
			s.AutoTravelRelocateDays, s.AutoTravelMonthlyDays = 6, 5
			s.AutoRegionEnable, s.AccessLogEnable = 1, 1
		}, false},
		// 长住改地区只在总开关开着时运行。升级前把每月放行天数设成 1~4 的管理员，升级后长住天数是默认的 5：
		// 总开关没开也一律拒绝的话，连改个端口都保存不了。
		{"总开关关着时不管长住天数与每月天数（升级陷阱）", func(s *AllSetting) { s.AutoTravelRelocateDays, s.AutoTravelMonthlyDays = 5, 3 }, true},
		{"关闭长住改地区时不管每月天数", func(s *AllSetting) { s.AutoTravelRelocateDays, s.AutoTravelMonthlyDays = 0, 1 }, true},
		// 离家后大约 8 − 稳定天数 天之内还能重新发通行证（近 7 天窗口里家里的天数），长住要到第 N 天才改：两者
		// 之和超过 8，中间就有几天通行证一断（续期失败、额度用完）既不能重发、也还没到改地区，人被一直拦着。
		{"总开关开着时稳定天数 + 长住天数超过 8", func(s *AllSetting) {
			s.AutoTravelStableDays, s.AutoTravelRelocateDays = 4, 5
			s.AutoRegionEnable, s.AccessLogEnable = 1, 1
		}, false},
		{"总开关开着时默认 3 + 5 = 8", func(s *AllSetting) { s.AutoRegionEnable, s.AccessLogEnable = 1, 1 }, true},
		{"总开关开着时稳定 1 + 长住 7", func(s *AllSetting) {
			s.AutoTravelStableDays, s.AutoTravelRelocateDays = 1, 7
			s.AutoRegionEnable, s.AccessLogEnable = 1, 1
		}, true},
		{"总开关关着时不管稳定天数 + 长住天数（升级陷阱）", func(s *AllSetting) { s.AutoTravelStableDays, s.AutoTravelRelocateDays = 5, 5 }, true},
		{"关闭长住改地区时不管稳定天数", func(s *AllSetting) {
			s.AutoTravelStableDays, s.AutoTravelRelocateDays = 7, 0
			s.AutoRegionEnable, s.AccessLogEnable = 1, 1
		}, true},
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

func TestCheckValidAutoRegionRanges(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*AllSetting)
		valid bool
	}{
		{"默认值", func(*AllSetting) {}, true},
		{"开关为 1（访问日志开着）", func(s *AllSetting) { s.AutoRegionEnable, s.AccessLogEnable = 1, 1 }, true},
		{"开关为 1 而访问日志关着", func(s *AllSetting) { s.AutoRegionEnable = 1 }, false},
		{"开关为 2", func(s *AllSetting) { s.AutoRegionEnable = 2 }, false},
		{"观察 0 天", func(s *AllSetting) { s.AutoRegionObserveDays = 0 }, false},
		{"观察 31 天", func(s *AllSetting) { s.AutoRegionObserveDays = 31 }, false},
		{"拦截上限 0%", func(s *AllSetting) { s.AutoRegionMaxBlockedPercent = 0 }, true},
		{"拦截上限 51%", func(s *AllSetting) { s.AutoRegionMaxBlockedPercent = 51 }, false},
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

// 防共享 / 防滥用的全局开关与阈值。
func TestCheckValidAntiAbuseRanges(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*AllSetting)
		valid bool
	}{
		{"默认值", func(*AllSetting) {}, true},
		{"机房拦截开关 2", func(s *AllSetting) { s.BlockIdcSources = 2 }, false},
		{"25 端口开关 2", func(s *AllSetting) { s.BlockSmtp = 2 }, false},
		{"流量识别补全开关 2", func(s *AllSetting) { s.ForceSniffing = 2 }, false},
		{"暴涨限速开关 2", func(s *AllSetting) { s.SpikeThrottleEnable = 2 }, false},
		{"暴涨倍数 1（等于平时也限速）", func(s *AllSetting) { s.SpikeThrottleMultiplier = 1 }, false},
		{"暴涨倍数 100", func(s *AllSetting) { s.SpikeThrottleMultiplier = 100 }, true},
		{"暴涨下限 0 GB", func(s *AllSetting) { s.SpikeThrottleFloorGB = 0 }, false},
		{"限速 0 Mbps（等于断网）", func(s *AllSetting) { s.SpikeThrottleMbps = 0 }, false},
		{"限速 1000 Mbps", func(s *AllSetting) { s.SpikeThrottleMbps = 1000 }, true},
		{"限速 1001 Mbps", func(s *AllSetting) { s.SpikeThrottleMbps = 1001 }, false},
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
