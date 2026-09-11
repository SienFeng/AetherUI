package service

import "testing"

func TestNetworkFamilyKey(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want string
	}{
		{"IPv4 归到 /24", "49.86.123.45", "49.86.123.0/24"},
		{"IPv4 同段的另一个地址归到同一个键", "49.86.123.200", "49.86.123.0/24"},
		{"IPv4 邻段不归并", "49.86.124.1", "49.86.124.0/24"},

		// RFC 8981 的临时地址只换后 64 位，所以 /64 必然把它们收拢。
		{"IPv6 归到 /64", "240e:3b1:2:abcd::1", "240e:3b1:2:abcd::/64"},
		{"IPv6 临时地址归到同一个键", "240e:3b1:2:abcd:dead:beef:1:2", "240e:3b1:2:abcd::/64"},
		{"IPv6 邻近 /64 不归并", "240e:3b1:2:abce::1", "240e:3b1:2:abce::/64"},

		// v4-mapped 必须折回四字节，与 online.normalizeIP 口径一致：同一个
		// 客户端不该因为内核给出的地址族表示不同而被算成两个来源。
		{"v4-mapped 折回 IPv4", "::ffff:49.86.123.45", "49.86.123.0/24"},

		{"非法输入返回空串", "not-an-ip", ""},
		{"空串返回空串", "", ""},
		{"带端口的地址不是合法 IP", "1.2.3.4:443", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := networkFamilyKey(c.ip); got != c.want {
				t.Errorf("networkFamilyKey(%q) = %q, want %q", c.ip, got, c.want)
			}
		})
	}
}

// 算不出网络族时必须回退原始地址而不是返回空串：空串会让一整批认不出的
// 地址塌缩成同一个键，那是把「几个未知来源」静默合并成「一个来源」。
func TestNetworkFamilyOrIPFallsBackToRawAddress(t *testing.T) {
	if got := networkFamilyOrIP("49.86.123.45"); got != "49.86.123.0/24" {
		t.Errorf("正常地址应返回网络族，得到 %q", got)
	}
	for _, raw := range []string{"not-an-ip", "also-not-an-ip", ""} {
		if got := networkFamilyOrIP(raw); got != raw {
			t.Errorf("networkFamilyOrIP(%q) = %q, want 原样返回", raw, got)
		}
	}
}

// 四个字段必须整组来自同一个数据源。跨源拼装会造出「江苏 + 上海市」这类
// 在界面上看得见的胡话，而画像键正是按这一组字段拼的。
func TestSelectNetworkMetaTakesOneSourceAsAWhole(t *testing.T) {
	t.Run("取第一个给出省份的源，整组用它的值", func(t *testing.T) {
		// 实测发生过的形态：ip2region 判北京、纯真判湖北（后者才对）。
		// 这里不解决谁对的问题——那是多源分歧，界面自己会提示；这里只保证
		// 不会把两个源的字段拼在一起。
		got := selectNetworkMeta([]sourceLocation{
			{Country: "中国", Region: "北京市", City: "北京市", ISP: "中国移动"},
			{Country: "中国", Region: "湖北省", City: "武汉市", ISP: "中国联通"},
		})
		want := NetworkMeta{Country: "中国", Province: "北京市", City: "北京市", ISP: "中国移动"}
		if got != want {
			t.Errorf("= %+v, want %+v", got, want)
		}
	})

	t.Run("没有省份时退到第一个给出国家的源", func(t *testing.T) {
		// 境外段就是这个形态：ipdb.normalize 只保留 Country 与知名 IDC 名。
		got := selectNetworkMeta([]sourceLocation{
			{Country: "美国", ISP: "Amazon"},
		})
		want := NetworkMeta{Country: "美国", ISP: "Amazon"}
		if got != want {
			t.Errorf("= %+v, want %+v", got, want)
		}
		if !got.HasGeo() {
			t.Error("境外段有 Country，必须算作有地理信息——只按 Province 判会把整个境外来源群误判成降级")
		}
	})

	t.Run("有省份的源排在境外源后面时仍然优先", func(t *testing.T) {
		got := selectNetworkMeta([]sourceLocation{
			{Country: "美国", ISP: "Amazon"},
			{Country: "中国", Region: "江苏省", City: "南通市", ISP: "中国电信"},
		})
		if got.Province != "江苏省" || got.Country != "中国" {
			t.Errorf("= %+v, want 江苏省那一组", got)
		}
	})

	t.Run("一个源都没有时返回零值且 HasGeo 为假", func(t *testing.T) {
		got := selectNetworkMeta(nil)
		if got != (NetworkMeta{}) || got.HasGeo() {
			t.Errorf("= %+v HasGeo=%v, want 零值/false", got, got.HasGeo())
		}
	})

	t.Run("主源没有 ISP 时宁可丢掉 ISP，也不从别的源拼一个过来", func(t *testing.T) {
		got := selectNetworkMeta([]sourceLocation{
			{Country: "中国", Region: "江苏省", City: "南通市"},
			{Country: "中国", Region: "上海市", ISP: "中国联通"},
		})
		if got.ISP != "" {
			t.Errorf("ISP = %q, want 空串——那个 ISP 属于另一个把该 IP 判到上海的源", got.ISP)
		}
	})
}
