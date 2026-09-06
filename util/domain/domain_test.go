package domain

import "testing"

// 期望值全部由 golang.org/x/net/publicsuffix v0.57.0 实测确认，不是推测。
func TestRegistrable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通三段域名", "www.speedtest.net:443", "speedtest.net"},
		{"多级子域名", "googleads.g.doubleclick.net:443", "doubleclick.net"},
		{"两段域名原样", "example.com:443", "example.com"},
		{"多级公共后缀", "example.co.uk:443", "example.co.uk"},
		{"github.io 是公共后缀", "a.b.c.example.github.io:443", "example.github.io"},
		{"大小写归一", "WWW.Example.COM:80", "example.com"},
		// 不剥尾点的话 EffectiveTLDPlusOne 报 "empty label"，会回落成
		// "example.com."，和 "example.com" 分裂成两个桶且没有任何报错。
		{"末尾点要剥掉", "example.com.:443", "example.com"},
		{"IPv4 字面量原样", "1.2.3.4:443", "1.2.3.4"},
		{"IPv6 字面量剥方括号", "[2001:db8::1]:443", "2001:db8::1"},
		{"本身就是公共后缀时原样", "com:443", "com"},
		{"无点主机名原样", "localhost:443", "localhost"},
		{"没有端口也要能处理", "example.com", "example.com"},
		{"空串", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Registrable(c.in); got != c.want {
				t.Errorf("Registrable(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

func TestIsRegistrable(t *testing.T) {
	// 计量池只能收真正的注册域名。放行公共后缀本身会生成 domain:com
	// 这种命中全部 .com 的规则，把该入站几乎全部流量吸进一个计量出站，
	// 榜单从此只有一行。
	yes := []string{
		"doubleclick.net",
		"example.co.uk",
		"9gag.com",
		"example.com.cn", // 多级公共后缀（com.cn）下的注册域名
	}
	for _, d := range yes {
		if !IsRegistrable(d) {
			t.Errorf("IsRegistrable(%q) = false，期望 true", d)
		}
	}
	no := []string{
		"",                        // 空
		"com",                     // 公共后缀本身
		"co.uk",                   // 多级公共后缀本身
		"localhost",               // 不含点的主机名
		"www.example.com",         // 子域名不是注册域名，池里只放归并后的结果
		"some-cdn.example.com.cn", // 多级公共后缀下的子域名，注册域名是 example.com.cn
		"1.2.3.4",                 // IPv4 字面量：需要 ip 条件而不是 domain 条件
		"2001:db8::1",             // IPv6 字面量
		"example.com.",            // 带末尾点：Registrable 已经剥过，这里不再兼容
		"EXAMPLE.COM",             // 大写：Registrable 已经转过小写，这里不再兼容
	}
	for _, d := range no {
		if IsRegistrable(d) {
			t.Errorf("IsRegistrable(%q) = true，期望 false", d)
		}
	}
}
