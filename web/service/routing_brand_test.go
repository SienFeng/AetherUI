package service

import "testing"

func TestDetectGroupBrand(t *testing.T) {
	cases := []struct {
		name    string
		remark  string
		domains []string
		want    string // 空串表示认不出
	}{
		// 预览前 5 条全是第三方依赖，openai 自己的域名排在后面也要认得出
		{"single brand among third parties", "ChatGPT", []string{
			"full:rum.browser-intake-datadoghq.com", "full:openai-api.arkoselabs.com",
			"full:cdn.auth0.com", "domain:openai.com", "domain:chatgpt.com"}, "openai"},
		{"no proportion threshold", "随便起的名字", []string{
			"domain:sentry.io", "domain:intercom.io", "domain:stripe.com", "domain:claude.ai"}, "claude"},
		{"remark arbitrates nested brands", "Gemini", []string{
			"full:gemini.google.com", "full:alkalimakersuite-pa.clients6.google.com",
			"domain:googleapis.com"}, "googlegemini"},
		{"multiple brands without hint", "网盘域名", []string{
			"domain:onedrive.com", "domain:apple-cloudkit.com", "domain:dropbox.com"}, ""},
		{"remark mentions two hit brands equally", "Google Gemini", []string{
			"full:gemini.google.com", "domain:google.com"}, ""},
		{"longer alias wins", "ChatGPT + Google", []string{
			"domain:openai.com", "domain:google.com"}, "openai"},
		{"label boundary", "x", []string{"domain:notopenai.com", "full:openai.com.evil.net"}, ""},
		{"keyword regexp geosite ignored", "OpenAI", []string{
			"keyword:openai", "regexp:.*openai.*", "geosite:openai"}, ""},
		{"case and trailing dot", "", []string{"full:API.OpenAI.com."}, "openai"},
		{"empty", "Netflix", nil, ""},
	}
	for _, c := range cases {
		got := DetectGroupBrand(c.remark, c.domains)
		slug := ""
		if got != nil {
			slug = got.Slug
		}
		if slug != c.want {
			t.Errorf("%s: got %q, want %q", c.name, slug, c.want)
		}
	}
}

// 后缀重复时 brandBySuffix 只留最后一个，前一个品牌的那条后缀静默失效。
func TestBrandSuffixesUnique(t *testing.T) {
	seen := map[string]string{}
	for _, b := range brandDefs {
		for _, s := range b.suffixes {
			if prev, ok := seen[s]; ok {
				t.Errorf("suffix %q listed under both %s and %s", s, prev, b.Slug)
			}
			seen[s] = b.Slug
		}
	}
}
