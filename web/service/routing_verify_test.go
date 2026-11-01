package service

import (
	"testing"

	"a-ui/database/model"
)

func TestSampleTargets(t *testing.T) {
	disp := func(ts []sampleTarget) []string {
		out := []string{}
		for _, s := range ts {
			out = append(out, s.Display)
		}
		return out
	}
	cases := []struct {
		name           string
		domains, cidrs []string
		perGroup       int
		want           []string
	}{
		{"域名优先且总数不超过 perGroup",
			[]string{"full:chatgpt.com", "domain:openai.com", "keyword:gpt"},
			[]string{"1.2.3.0/24", "!10.0.0.0/8", "geoip:us"}, 2,
			[]string{"chatgpt.com", "openai.com"}},
		{"域名不足时补 IP", []string{"keyword:gpt"}, []string{"1.2.3.0/24"}, 2, []string{"1.2.3.1"}},
		{"IPv6 段", nil, []string{"2001:db8::/32"}, 2, []string{"2001:db8::1"}},
		{"/32 取自身", nil, []string{"9.9.9.9/32"}, 2, []string{"9.9.9.9"}},
		{"纯 IP 字面量", nil, []string{"8.8.8.8"}, 2, []string{"8.8.8.8"}},
		{"取反与 geoip/ext 跳过", nil, []string{"!10.0.0.0/8", "geoip:us", "ext:a.dat:cn"}, 2, []string{}},
		{"不可推样例的域名写法", []string{"keyword:a", "regexp:^a$", "geosite:openai", "dotless:x", "ext:f:t"}, nil, 2, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := disp(sampleTargets(c.domains, c.cidrs, c.perGroup))
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v want %v", got, c.want)
				}
			}
		})
	}
	ts := sampleTargets([]string{"full:a.com"}, []string{"1.2.3.0/24"}, 2)
	if ts[0].Domain != "a.com" || ts[0].IP != nil || ts[1].IP.String() != "1.2.3.1" || ts[1].Domain != "" {
		t.Fatalf("字段不对: %+v", ts)
	}
}

func TestJudgeSampleProxy(t *testing.T) {
	exp := ExpectedOutbound{Action: model.ActionProxy, Tag: "a-ui-us"}
	cases := []struct {
		actual         string
		noMatch        bool
		match, hitRule bool
	}{
		{"a-ui-us", false, true, true},
		{model.DefaultOutboundTag, false, false, true},
		{"", true, false, false},
	}
	for _, c := range cases {
		m, h := judgeSample(exp, c.actual, c.noMatch, model.DefaultOutboundTag)
		if m != c.match || h != c.hitRule {
			t.Errorf("actual=%q noMatch=%v got (%v,%v)", c.actual, c.noMatch, m, h)
		}
	}
}

func TestJudgeSampleBlock(t *testing.T) {
	exp := ExpectedOutbound{Action: model.ActionBlock, Tag: model.BlockOutboundTag}
	m, h := judgeSample(exp, model.BlockOutboundTag, false, model.DefaultOutboundTag)
	if !m || !h {
		t.Fatalf("got (%v,%v)", m, h)
	}
	m, h = judgeSample(exp, "", true, model.DefaultOutboundTag)
	if m || h {
		t.Fatalf("noMatch got (%v,%v)", m, h)
	}
}

func TestJudgeSampleDirect(t *testing.T) {
	exp := ExpectedOutbound{Action: model.ActionDirect, Tag: model.DefaultOutboundTag}
	cases := []struct {
		actual         string
		noMatch        bool
		match, hitRule bool
	}{
		{model.DefaultOutboundTag, false, true, true},
		{"", true, true, false},
		{"a-ui-meter-openai-com", false, true, false},
		{"a-ui-us", false, false, true},
	}
	for _, c := range cases {
		m, h := judgeSample(exp, c.actual, c.noMatch, model.DefaultOutboundTag)
		if m != c.match || h != c.hitRule {
			t.Errorf("actual=%q noMatch=%v got (%v,%v)", c.actual, c.noMatch, m, h)
		}
	}
}

func TestRouteHead(t *testing.T) {
	for in, want := range map[string]string{
		"a-ui-us -> a-ui-default": "a-ui-us",
		"a-ui-us >> x":            "a-ui-us",
		"direct":                  "direct",
		"":                        "",
		"  a  -> b":               "a",
	} {
		if got := routeHead(in); got != want {
			t.Errorf("routeHead(%q)=%q want %q", in, got, want)
		}
	}
}

func TestCombineVerdict(t *testing.T) {
	ok := SampleResult{Target: "a.com", Match: true, HitByRule: true}
	bad := SampleResult{Target: "b.com", Match: false, HitByRule: true}
	errS := SampleResult{Target: "c.com", Error: "超时"}
	others := &LogEvidence{Matched: 3, Others: []LogOther{{Route: "a-ui-default", Count: 1}}}
	cases := []struct {
		name    string
		state   string
		samples []SampleResult
		log     *LogEvidence
		verdict string
		reason  string // 非空则要求完全相等
	}{
		{"样例不符", "ok", []SampleResult{ok, bad}, nil, "mismatch", ""},
		{"日志有其他出站即使核心全符合", "ok", []SampleResult{ok}, others, "mismatch", ""},
		{"pending 无不符", "pending", []SampleResult{ok}, nil, "pending", ""},
		{"pending 且样例不符仍为 pending", "pending", []SampleResult{bad}, nil, "pending", ""},
		{"pending 且日志有其他出站仍为 pending", "pending", []SampleResult{ok}, others, "pending", ""},
		{"全部符合", "ok", []SampleResult{ok, ok}, nil, "verified", ""},
		{"全部符合带日志", "ok", []SampleResult{ok}, &LogEvidence{Matched: 5}, "verified", ""},
		{"无样例", "ok", nil, nil, "unverifiable", "本规则的域名都是关键词/正则/geosite 写法，推不出可测试的域名"},
		{"样例全部出错", "ok", []SampleResult{errS, errS}, nil, "unverifiable", ""},
		{"部分样例出错不能变绿", "ok", []SampleResult{ok, errS}, nil, "unverifiable", ""},
		{"出错样例不算不符", "ok", []SampleResult{errS}, nil, "unverifiable", ""},
		{"核心未运行", "not_running", nil, nil, "unverifiable", "xray 未运行"},
		{"核心忙", "busy", nil, nil, "unverifiable", "核心正在更新配置，请稍后重新验证"},
		{"仅日志不能变绿", "ok", nil, &LogEvidence{Matched: 9}, "unverifiable", ""},
		{"核心未运行时样例不信任", "not_running", []SampleResult{ok}, &LogEvidence{Matched: 9}, "unverifiable", "xray 未运行"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, r := combineVerdict(c.state, c.samples, c.log)
			if v != c.verdict {
				t.Fatalf("verdict=%q(%s) want %q", v, r, c.verdict)
			}
			if c.reason != "" && r != c.reason {
				t.Fatalf("reason=%q want %q", r, c.reason)
			}
			if v != "verified" && r == "" {
				t.Fatalf("非 verified 必须带 reason")
			}
		})
	}
}
