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
