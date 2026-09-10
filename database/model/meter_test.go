package model

import "testing"

func TestMeterTagRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		inboundId int
		domain    string
	}{
		{"普通域名", 3, "doubleclick.net"},
		{"域名含短横线", 12, "some-cdn.example.com"},
		{"多级注册域名", 7, "example.co.uk"},
		{"入站 id 多位", 12345, "a.io"},
		{"域名首字符是数字", 1, "9gag.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tag := MeterTag(c.inboundId, c.domain)
			gotId, gotDomain, ok := ParseMeterTag(tag)
			if !ok {
				t.Fatalf("ParseMeterTag(%q) 判为不可解析", tag)
			}
			if gotId != c.inboundId || gotDomain != c.domain {
				t.Errorf("反查 = (%d, %q)，期望 (%d, %q)", gotId, gotDomain, c.inboundId, c.domain)
			}
		})
	}
}

func TestParseMeterTagRejectsMalformed(t *testing.T) {
	// 每一条都必须被拒绝：一个形态不对的 tag 是无法归因的，
	// 硬猜只会把字节记到错的域名上。
	bad := []string{
		"",                    // 空
		"a-ui-block",          // 别的保留 tag
		"a-ui-hk",             // 出站节点的 tag
		"inbound-2886",        // 入站 tag
		"a-ui-meter-",         // 只有前缀
		"a-ui-meter-3",        // 缺分隔符与域名
		"a-ui-meter-3-",       // 域名为空
		"a-ui-meter--x.com",   // id 为空
		"a-ui-meter-x-y.com",  // id 不是数字
		"a-ui-meter-0-x.com",  // id 必须为正：0 不是任何入站
		"a-ui-meter--1-x.com", // 同上：首字符就是分隔符，切出来的 id 仍是空串
	}
	for _, tag := range bad {
		if _, _, ok := ParseMeterTag(tag); ok {
			t.Errorf("ParseMeterTag(%q) 判为可解析，期望拒绝", tag)
		}
	}
}

func TestIsReservedTagRejectsMeterPrefix(t *testing.T) {
	// 保留 tag 不在 outbound_nodes 表里，数据库唯一约束看不见它们；
	// 撞名会让 xray 报 existing tag found 并拒绝启动整份配置——全员断网，
	// 而面板首页仍显示 running。
	reserved := []string{
		BlockOutboundTag,
		DefaultOutboundTag,
		"a-ui-meter-3-doubleclick.net",
		"a-ui-meter-",       // 光是前缀也要拒绝：它不可能是一个合法节点 tag
		"a-ui-meter-乱七八糟", // 形态不对但仍带前缀，同样不能分配出去
	}
	for _, tag := range reserved {
		if !IsReservedTag(tag) {
			t.Errorf("IsReservedTag(%q) = false，期望 true", tag)
		}
	}
	for _, tag := range []string{"a-ui-hk", "a-ui-meterx", "blocked", "meter-3-x.com", ""} {
		if IsReservedTag(tag) {
			t.Errorf("IsReservedTag(%q) = true，期望 false", tag)
		}
	}
}
