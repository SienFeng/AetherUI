package entity

import "a-ui/util/common"

// checkAntiAbuse 校验防共享 / 防滥用的开关与阈值。
func (s *AllSetting) checkAntiAbuse() error {
	switches := []struct {
		name string
		v    int
	}{
		{"拦截机房 / 云服务器来源", s.BlockIdcSources},
		{"拦截 25 端口发信", s.BlockSmtp},
		{"补全流量识别", s.ForceSniffing},
		{"流量暴涨自动限速", s.SpikeThrottleEnable},
	}
	for _, sw := range switches {
		if sw.v != 0 && sw.v != 1 {
			return common.NewError(sw.name, "开关只能是 0 或 1:", sw.v)
		}
	}
	ranges := []struct {
		name     string
		v        int
		min, max int
	}{
		// 倍数 1 等于「和平时一样多就限速」。
		{"流量暴涨：倍数", s.SpikeThrottleMultiplier, 2, 100},
		{"流量暴涨：每天至少多少 GB 才算", s.SpikeThrottleFloorGB, 1, 10000},
		// 0 Mbps 等于断网，那是停用，不是限速。
		{"流量暴涨：限速到多少 Mbps", s.SpikeThrottleMbps, 1, 1000},
	}
	for _, r := range ranges {
		if r.v < r.min || r.v > r.max {
			return common.NewError(r.name, "应在", r.min, "~", r.max, "之间:", r.v)
		}
	}
	return nil
}
