package service

import (
	"testing"
	"time"
)

// DB-IP 每月 1 日发布新一版，文件名里带年月，所以下载地址不是固定串。
// 往前踩空（用一个还没发布的月份）会连着几天 404，而界面上只显示「更新失败」，
// 看不出是在等上游发布；往回退一个月则永远安全——实测旧月份的地址不会下线。
func TestExpandMonthPlaceholder(t *testing.T) {
	const tmpl = "https://download.db-ip.com/free/dbip-city-lite-{YYYY-MM}.csv.gz"
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{
			name: "月中用当月",
			now:  time.Date(2026, 9, 13, 4, 0, 0, 0, time.UTC),
			want: "https://download.db-ip.com/free/dbip-city-lite-2026-09.csv.gz",
		},
		{
			name: "3 日起用当月",
			now:  time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
			want: "https://download.db-ip.com/free/dbip-city-lite-2026-09.csv.gz",
		},
		{
			name: "1 日退回上月",
			now:  time.Date(2026, 9, 1, 23, 59, 0, 0, time.UTC),
			want: "https://download.db-ip.com/free/dbip-city-lite-2026-08.csv.gz",
		},
		{
			name: "2 日退回上月",
			now:  time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
			want: "https://download.db-ip.com/free/dbip-city-lite-2026-08.csv.gz",
		},
		{
			// 跨年是最容易写错的一档：退一个月同时要退一年。
			name: "1 月 1 日退回上一年 12 月",
			now:  time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			want: "https://download.db-ip.com/free/dbip-city-lite-2026-12.csv.gz",
		},
		{
			// 3 月 1 日退回 2 月，而 2 月只有 28/29 天——用 AddDate(0,-1,0) 会在
			// 这里踩坑，所以实现是「减掉当月已过的天数」而不是「减一个月」。
			name: "3 月 1 日退回 2 月",
			now:  time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			want: "https://download.db-ip.com/free/dbip-city-lite-2026-02.csv.gz",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expandMonthPlaceholder(tmpl, c.now); got != c.want {
				t.Errorf("= %q\n想要 %q", got, c.want)
			}
		})
	}
}

// 其余三个源的地址是固定串，一个字节都不能被改动——尤其 IP2Location 的地址里
// 带着管理员的 token，任何意外改写都会让下载 403 而看不出原因。
func TestExpandMonthPlaceholderLeavesPlainURLsAlone(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, raw := range []string{
		"https://raw.githubusercontent.com/lionsoul2014/ip2region/master/data/ipv4_source.txt",
		"https://raw.githubusercontent.com/FW27623/qqwry/main/qqwry.dat",
		"https://www.ip2location.com/download?token=ABC123&file=DB3LITE",
		"",
	} {
		if got := expandMonthPlaceholder(raw, now); got != raw {
			t.Errorf("expandMonthPlaceholder(%q) = %q；不含占位符时必须原样返回", raw, got)
		}
	}
}
