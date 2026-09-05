package service

import "a-ui/web/entity"

// validBaseSetting 返回一份**当前所有校验都能通过**的配置，供各处针对
// 单个字段的校验测试作基线。
//
// 存在的理由：CheckValid 是逐条串行校验的，前面任何一项不合法都会让
// 后面的项根本走不到。每个测试各写一份基线的话，只要新增一条校验规则，
// 所有基线都会一起失效，报错还指向一个与该测试无关的字段——这个坑已经
// 踩过三次了。新增设置项时**只需要在这里补一个合法值**。
func validBaseSetting() *entity.AllSetting {
	return &entity.AllSetting{
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
