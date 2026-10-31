package service

import (
	"strings"
)

// GroupBrand 是分流页域名组卡片左上角显示的厂商标识。Slug 同时是
// web/assets/brand-icons/<slug>.svg 的文件名（TestBrandIconsExist 守着）；
// 图标是全彩原样的官方样式，界面不再另配底色。
type GroupBrand struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

type brandDef struct {
	GroupBrand
	// 按 label 边界匹配的后缀：openai.com 命中 chat.openai.com，不命中 notopenai.com。
	suffixes []string
	// 备注里出现这些词（不区分大小写）时，用来在多个命中的品牌之间仲裁。
	// 不收太短的词：「line」会命中 online，「x」「max」什么都会命中。
	aliases []string
}

// brandDefs 只是展示用的识别表，不参与任何分流判定。
//
// 有包含关系的品牌（gemini.google.com 之于 google.com）靠最长后缀分开计数，
// 一个 Gemini 组因此会同时命中 Gemini 与 Google，交给备注仲裁。
var brandDefs = []brandDef{
	// ── AI ──
	{GroupBrand{"openai", "OpenAI"},
		[]string{"openai.com", "chatgpt.com", "chat.com", "oaistatic.com", "oaiusercontent.com", "sora.com"},
		[]string{"openai", "chatgpt", "gpt", "sora"}},
	{GroupBrand{"claude", "Claude"},
		[]string{"anthropic.com", "claude.ai", "claude.com", "clau.de", "claudeusercontent.com"},
		[]string{"claude", "anthropic"}},
	{GroupBrand{"googlegemini", "Gemini"},
		[]string{"gemini.google.com", "gemini.google", "aistudio.google.com", "makersuite.google.com",
			"generativelanguage.googleapis.com", "ai.google.dev", "bard.google.com",
			"deepmind.com", "deepmind.google", "notebooklm.google", "notebooklm.google.com"},
		[]string{"gemini", "bard", "aistudio", "ai studio", "notebooklm"}},
	{GroupBrand{"grok", "Grok"},
		[]string{"grok.com", "x.ai"},
		[]string{"grok", "xai"}},
	{GroupBrand{"deepseek", "DeepSeek"},
		[]string{"deepseek.com"},
		[]string{"deepseek"}},
	{GroupBrand{"perplexity", "Perplexity"},
		[]string{"perplexity.ai", "pplx.ai"},
		[]string{"perplexity"}},
	{GroupBrand{"mistralai", "Mistral AI"},
		[]string{"mistral.ai"},
		[]string{"mistral"}},
	{GroupBrand{"copilot", "Copilot"},
		[]string{"copilot.microsoft.com", "copilot.cloud.microsoft"},
		[]string{"copilot"}},
	{GroupBrand{"qwen", "通义千问"},
		[]string{"qwen.ai", "qwenlm.ai", "tongyi.aliyun.com", "qianwen.aliyun.com"},
		[]string{"qwen", "通义", "千问"}},
	{GroupBrand{"kimi", "Kimi"},
		[]string{"kimi.com", "kimi.ai", "moonshot.cn", "moonshot.ai"},
		[]string{"kimi", "moonshot", "月之暗面"}},
	{GroupBrand{"doubao", "豆包"},
		[]string{"doubao.com"},
		[]string{"doubao", "豆包"}},
	{GroupBrand{"cursor", "Cursor"},
		[]string{"cursor.com", "cursor.sh", "cursorapi.com", "cursor-cdn.com"},
		[]string{"cursor"}},
	{GroupBrand{"poe", "Poe"},
		[]string{"poe.com"},
		[]string{"poe"}},
	{GroupBrand{"huggingface", "Hugging Face"},
		[]string{"huggingface.co", "hf.co"},
		[]string{"huggingface", "hugging face"}},

	// ── 大厂与常见服务 ──
	{GroupBrand{"google", "Google"},
		[]string{"google.com", "googleapis.com", "gstatic.com", "googleusercontent.com", "google.dev",
			"withgoogle.com", "gmail.com", "ggpht.com"},
		[]string{"google", "谷歌"}},
	{GroupBrand{"youtube", "YouTube"},
		[]string{"youtube.com", "youtu.be", "ytimg.com", "googlevideo.com", "youtube-nocookie.com",
			"youtubei.googleapis.com"},
		[]string{"youtube", "油管"}},
	{GroupBrand{"microsoft", "Microsoft"},
		[]string{"microsoft.com", "live.com", "live.net", "office.com", "office.net", "onedrive.com",
			"cloud.microsoft", "msn.com", "bing.com", "sharepoint.com", "outlook.com",
			"microsoftonline.com", "windows.net", "azure.com", "1drv.ms"},
		[]string{"microsoft", "微软", "onedrive", "outlook", "office", "bing"}},
	{GroupBrand{"apple", "Apple"},
		[]string{"apple.com", "icloud.com", "apple-cloudkit.com", "mzstatic.com", "cdn-apple.com",
			"apple.news", "icloud-content.com"},
		[]string{"apple", "苹果", "icloud"}},
	{GroupBrand{"netflix", "Netflix"},
		[]string{"netflix.com", "netflix.net", "nflxvideo.net", "nflximg.net", "nflximg.com",
			"nflxext.com", "nflxso.net"},
		[]string{"netflix", "奈飞", "网飞"}},
	{GroupBrand{"speedtest", "Speedtest"},
		[]string{"speedtest.net", "ookla.com", "ooklaserver.net"},
		[]string{"speedtest", "ookla"}},
	{GroupBrand{"telegram", "Telegram"},
		[]string{"telegram.org", "t.me", "telegram.me", "telesco.pe", "tdesktop.com", "telegra.ph"},
		[]string{"telegram", "电报"}},
	{GroupBrand{"x", "X"},
		[]string{"x.com", "twitter.com", "twimg.com", "t.co"},
		[]string{"twitter", "推特"}},
	{GroupBrand{"github", "GitHub"},
		[]string{"github.com", "githubusercontent.com", "githubassets.com", "github.io", "ghcr.io"},
		[]string{"github"}},
	{GroupBrand{"spotify", "Spotify"},
		[]string{"spotify.com", "scdn.co", "spotifycdn.com"},
		[]string{"spotify"}},
	{GroupBrand{"tiktok", "TikTok"},
		[]string{"tiktok.com", "tiktokv.com", "tiktokcdn.com", "tiktokcdn-us.com"},
		[]string{"tiktok"}},
	{GroupBrand{"instagram", "Instagram"},
		[]string{"instagram.com", "cdninstagram.com"},
		[]string{"instagram"}},
	{GroupBrand{"facebook", "Facebook"},
		[]string{"facebook.com", "fbcdn.net", "fb.com", "facebook.net"},
		[]string{"facebook"}},
	{GroupBrand{"whatsapp", "WhatsApp"},
		[]string{"whatsapp.com", "whatsapp.net", "wa.me"},
		[]string{"whatsapp"}},
	{GroupBrand{"discord", "Discord"},
		[]string{"discord.com", "discord.gg", "discordapp.com", "discordapp.net", "discord.media"},
		[]string{"discord"}},
	{GroupBrand{"steam", "Steam"},
		[]string{"steampowered.com", "steamcommunity.com", "steamstatic.com", "steamcontent.com"},
		[]string{"steam"}},
	{GroupBrand{"cloudflare", "Cloudflare"},
		[]string{"cloudflare.com", "cloudflare-dns.com", "one.one.one.one"},
		[]string{"cloudflare"}},
	{GroupBrand{"twitch", "Twitch"},
		[]string{"twitch.tv", "ttvnw.net", "jtvnw.net"},
		[]string{"twitch"}},
	{GroupBrand{"reddit", "Reddit"},
		[]string{"reddit.com", "redd.it", "redditmedia.com", "redditstatic.com"},
		[]string{"reddit"}},
	{GroupBrand{"wikipedia", "Wikipedia"},
		[]string{"wikipedia.org", "wikimedia.org"},
		[]string{"wikipedia", "维基"}},
	{GroupBrand{"paypal", "PayPal"},
		[]string{"paypal.com", "paypalobjects.com"},
		[]string{"paypal"}},
	{GroupBrand{"dropbox", "Dropbox"},
		[]string{"dropbox.com", "dropboxapi.com", "dropboxusercontent.com"},
		[]string{"dropbox"}},
	{GroupBrand{"bilibili", "哔哩哔哩"},
		[]string{"bilibili.com", "bilivideo.com", "hdslb.com", "biliapi.net", "b23.tv"},
		[]string{"bilibili", "哔哩", "b站"}},
	{GroupBrand{"docker", "Docker"},
		[]string{"docker.com", "docker.io"},
		[]string{"docker"}},
	{GroupBrand{"playstation", "PlayStation"},
		[]string{"playstation.com", "playstation.net", "sonyentertainmentnetwork.com"},
		[]string{"playstation", "psn"}},
	{GroupBrand{"hbomax", "HBO Max"},
		[]string{"hbomax.com", "max.com", "hbo.com"},
		[]string{"hbo"}},
	{GroupBrand{"crunchyroll", "Crunchyroll"},
		[]string{"crunchyroll.com"},
		[]string{"crunchyroll"}},
	{GroupBrand{"notion", "Notion"},
		[]string{"notion.so", "notion.site", "notion.com"},
		[]string{"notion"}},
	{GroupBrand{"zoom", "Zoom"},
		[]string{"zoom.us", "zoom.com"},
		[]string{"zoom"}},
}

// brandBySuffix 由 brandDefs 展开，后缀在表里重复是写错了（TestBrandSuffixesUnique 守着）。
var brandBySuffix = func() map[string]int {
	m := make(map[string]int)
	for i, b := range brandDefs {
		for _, s := range b.suffixes {
			m[s] = i
		}
	}
	return m
}()

// DetectGroupBrand 按域名组的完整域名列表（手工 + 订阅合并后）认出它属于哪个厂商，
// 认不出返回 nil，界面回落首字母。只用于展示。
//
// 只看 domain: / full: 两种写法：keyword: / regexp: 认不出确定的域名，geosite:
// 是一整类域名（geosite:cn 什么都有）。每条按最长后缀归到一个品牌：
//   - 只命中一个品牌 → 就是它。不设「占总条数比例」的门槛：Claude 组 160 条里
//     只有几条是 anthropic/claude 自己的域名，其余全是它依赖的第三方服务。
//   - 命中多个品牌 → 看备注里提到了其中哪一个（Gemini 组同时命中 Gemini 与
//     Google）；备注提到的不止一个时取别名最长的那个，仍分不出就不贴。
//     中国域名列表这类大杂烩组会命中几十个品牌，备注又不提任何一个，正好落空。
func DetectGroupBrand(remark string, domains []string) *GroupBrand {
	hit := make([]bool, len(brandDefs))
	var hits []int
	for _, d := range domains {
		host, ok := brandHost(d)
		if !ok {
			continue
		}
		i, ok := lookupBrand(host)
		if !ok || hit[i] {
			continue
		}
		hit[i] = true
		hits = append(hits, i)
	}
	if len(hits) == 0 {
		return nil
	}
	if len(hits) == 1 {
		b := brandDefs[hits[0]].GroupBrand
		return &b
	}

	name := strings.ToLower(remark)
	best, bestLen, tie := -1, 0, false
	for _, i := range hits {
		for _, a := range brandDefs[i].aliases {
			if !strings.Contains(name, a) {
				continue
			}
			switch {
			case len(a) > bestLen:
				best, bestLen, tie = i, len(a), false
			case len(a) == bestLen && i != best:
				tie = true
			}
		}
	}
	if best < 0 || tie {
		return nil
	}
	b := brandDefs[best].GroupBrand
	return &b
}

// brandHost 取出 domain: / full: 条目里的域名，其余写法一律不参与识别。
func brandHost(entry string) (string, bool) {
	for _, p := range []string{"domain:", "full:"} {
		if strings.HasPrefix(entry, p) {
			return strings.ToLower(strings.TrimSuffix(entry[len(p):], ".")), true
		}
	}
	return "", false
}

// lookupBrand 从完整域名开始逐级去掉最左边的 label，第一个命中的就是最长后缀。
func lookupBrand(host string) (int, bool) {
	for host != "" {
		if i, ok := brandBySuffix[host]; ok {
			return i, true
		}
		dot := strings.IndexByte(host, '.')
		if dot < 0 {
			break
		}
		host = host[dot+1:]
	}
	return 0, false
}

// BrandSlugs 列出识别表里的全部 slug，供测试核对每个都有对应的图标文件。
func BrandSlugs() []string {
	out := make([]string, len(brandDefs))
	for i, b := range brandDefs {
		out[i] = b.Slug
	}
	return out
}
