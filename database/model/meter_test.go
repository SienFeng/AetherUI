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

// B 类计量 tag 的形态与反查。ParseMeterTag 对它返回的第二个值是 rule:<id>，
// 这样 RecordMetered 不用改一行——它只是把反查出来的键原样写进 DomainStat.Domain。
func TestMeterRuleTagRoundTrip(t *testing.T) {
	tag := MeterRuleTag(7, 9)
	if tag != "a-ui-meter-r-7-9" {
		t.Fatalf("MeterRuleTag = %q", tag)
	}
	if !IsMeterTag(tag) {
		t.Error("B 类 tag 必须仍被 IsMeterTag 认作计量 tag，否则四道 fail-close 防线漏掉它")
	}
	id, key, ok := ParseMeterTag(tag)
	if !ok || id != 7 || key != "rule:9" {
		t.Errorf("ParseMeterTag(%q) = (%d, %q, %v)，期望 (7, \"rule:9\", true)", tag, id, key, ok)
	}
	if !IsRuleStatKey(key) {
		t.Errorf("IsRuleStatKey(%q) = false", key)
	}
	if rid, ok := ParseRuleStatKey(key); !ok || rid != 9 {
		t.Errorf("ParseRuleStatKey(%q) = (%d, %v)", key, rid, ok)
	}
}

// A 类 tag 的反查一个字节不变——这是回归守卫。
func TestParseMeterTagStillHandlesDomainAndIP(t *testing.T) {
	cases := []struct{ tag, wantKey string }{
		{"a-ui-meter-7-google.com", "google.com"},
		{"a-ui-meter-7-some-cdn.example.com", "some-cdn.example.com"}, // 域名含短横线
		{"a-ui-meter-7-72.235.209.83", "72.235.209.83"},
		{"a-ui-meter-7-2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		id, key, ok := ParseMeterTag(c.tag)
		if !ok || id != 7 || key != c.wantKey {
			t.Errorf("ParseMeterTag(%q) = (%d, %q, %v)，期望 (7, %q, true)", c.tag, id, key, ok, c.wantKey)
		}
	}
}

// 形态不对的 B 类 tag 一律拒绝，绝不猜。
func TestParseMeterTagRejectsMalformedRuleTags(t *testing.T) {
	for _, tag := range []string{
		"a-ui-meter-r-7",   // 缺规则 id
		"a-ui-meter-r-7-",  // 规则 id 为空
		"a-ui-meter-r-7-x", // 规则 id 非数字
		"a-ui-meter-r-0-9", // 入站 id 为 0
		"a-ui-meter-r-7-0", // 规则 id 为 0
		"a-ui-meter-r--9",  // 入站 id 为空
	} {
		if _, _, ok := ParseMeterTag(tag); ok {
			t.Errorf("ParseMeterTag(%q) 应该拒绝", tag)
		}
	}
}

// rule: 前缀不可能与真实目标撞车：Domain 列的值全部来自 domain.Registrable，
// 它的输出是注册域名或 IP。这条测试守的是 IsRuleStatKey 对真实目标的否定判定。
func TestIsRuleStatKeyRejectsRealTargets(t *testing.T) {
	for _, s := range []string{"google.com", "72.235.209.83", "2001:db8::1", "rule", "rule:", "rule:x", ""} {
		if IsRuleStatKey(s) {
			t.Errorf("IsRuleStatKey(%q) = true", s)
		}
	}
}
