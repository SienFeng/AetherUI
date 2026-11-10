package service

import (
	"net"

	"github.com/xtls/xray-core/app/router"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/geodata"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/xray"
)

// geoBlockMatcher 判断访问日志里一条走了 a-ui-block 的连接，是不是被入站的地区限制拦下的。
//
// 为什么流量走向验证要认它：地区规则（rulesFromGeoPlan）排在所有分流规则之前，来源 IP
// 不在允许集里的连接，不论访问什么都直接进黑洞。这些连接从未轮到任何分流规则，算作
// 「本规则走错」是误报——而且会让覆盖这个用户的每一条规则同时变红。
//
// 判定不手写：条件取自 geoRuleSources（与规则生成同一份），交给 xray 自己的
// geodata.ParseIPRules + router.NewIPMatcher(…, MatcherAsType_Source)，与核心解析
// routing.rules[].source 的路径一致；允许集读的是核心同一个 bin/a-ui-geo.dat。
//
// 按**核心正在用的**地区规则判断（appliedGeo）：dat 代码取已生效的那份（按内容寻址，xray 的「文件:代码」
// 缓存不会给出旧集合），锁死的入站（a-ui-geo-closed-*，不引用 dat）所有来源都算被拦——修复前它们匹配器
// 建不出、一律算「走错了出站」，覆盖这个用户的每条分流规则都变红。
type geoBlockMatcher struct {
	// byInbound 是配了地区限制的启用入站 → 它那两条拒绝规则的 source 匹配器。
	byInbound map[int]*geoSourceRules
	// closed 是核心里被锁死的入站：来源一律进黑洞。
	closed map[int]bool
	// storedAllow 只给持有通行证的入站：它**存储的**地区对应的允许集。
	//
	// 调用方大多在回放过去 24 小时的访问日志，而匹配器只能按此刻的有效地区构建。发证
	// 之前那些被地区规则拦下的行，按发证后的有效地区看就不再「被拦」，会被算成走错了
	// 出站，覆盖这个用户的每条分流规则都会变红——自动出差放行会让这种事反复发生。
	// 存储的地区 ⊆ 任何时刻的有效地区，所以「在存储的地区之外」覆盖了窗口内任一时刻
	// 会被地区规则拦下的来源。
	storedAllow map[int]*cidrSet
}

type geoSourceRules struct {
	v6, outside *router.IPMatcher
}

// newGeoBlockMatcher 为配了地区限制的启用入站构建匹配器。某个入站的匹配器建不出（例如核心还没用上过
// 它的地区规则）时只跳过那个入站并记日志：那个入站的封禁连接退回原来的口径（计入「走了其他出站」），
// 宁可多报也不把真正走错的连接藏起来。
//
// 判断按核心正在用的地区规则（有效地区 = 存储的地区 ∪ 活动通行证，在下发那一刻算好的）。
func newGeoBlockMatcher(inbounds []*model.Inbound) *geoBlockMatcher {
	return newGeoBlockMatcherWith(inbounds, activePassesOrNil(), allowedSetFor)
}

// newLiveGeoBlockMatcher 只按核心**此刻**生效的地区规则判断，不带 storedAllow 那条回放分支。给判断「这个来源
// 现在会不会被挡在门外」的调用方（并发判定）用：storedAllow 把持有通行证的入站在存储地区之外的来源一律算成
// 被拦，那是为回放 24 小时日志写的；并发判定复用它的话，通行证有效期间通行证省份的网络全部不占名额。
// passes 由调用方传入：并发判定同一轮还要拿它决定哪些入站要排「本人常住省优先」（homeNetworks）。
func newLiveGeoBlockMatcher(inbounds []*model.Inbound, passes map[int]*model.InboundRegionPass) *geoBlockMatcher {
	return newGeoBlockMatcherWith(inbounds, passes, nil)
}

// storedAllowOf 构造存储地区的允许集；测试里可替换，免得依赖真实 IP 库。传 nil 表示不做存储地区的回放
// （见 newLiveGeoBlockMatcher）。
func newGeoBlockMatcherWith(inbounds []*model.Inbound, passes map[int]*model.InboundRegionPass,
	storedAllowOf func([]string) (*cidrSet, error)) *geoBlockMatcher {
	g := &geoBlockMatcher{byInbound: map[int]*geoSourceRules{}, closed: map[int]bool{}, storedAllow: map[int]*cidrSet{}}
	applied := appliedGeo()
	byCode := map[string]*geoSourceRules{}
	// 核心还没用上过某个入站的地区规则（面板刚启动、或这个入站刚配上地区）时，退回按现有 dat 里同一组
	// 省份的条目判断——与改动前一样。只在需要时读一次文件。
	var datCodeOfTag map[string]string
	fallbackCode := func(in *model.Inbound) string {
		provinces, suspended, err := effectiveRegions(in, passes[in.Id])
		if err != nil || suspended || len(provinces) == 0 {
			return ""
		}
		if datCodeOfTag == nil {
			datCodeOfTag = map[string]string{}
			if entries, _, err := readGeoDat(geoDatPath); err == nil {
				for _, e := range entries {
					if tag := geoTagOfCode(e.Tag); datCodeOfTag[tag] == "" {
						datCodeOfTag[tag] = e.Tag
					}
				}
			}
		}
		return datCodeOfTag[regionTag(provinces)]
	}
	for _, in := range inbounds {
		if !in.Enable {
			continue // 停用入站不在核心配置里，也没有它的地区规则（buildGeoPlan 同样跳过）
		}
		if passes[in.Id] != nil && storedAllowOf != nil {
			if stored, err := DecodeRegions(in.Regions); err == nil && len(stored) > 0 {
				if set, err := storedAllowOf(stored); err == nil {
					g.storedAllow[in.Id] = set
				}
			}
		}
		if applied.Degraded[in.Id].Mode == geoDegradeClosed {
			g.closed[in.Id] = true
			continue
		}
		code, ok := applied.Codes[in.Id]
		if !ok {
			if code = fallbackCode(in); code == "" {
				continue
			}
		}
		rules, ok := byCode[code]
		if !ok {
			var err error
			rules, err = buildGeoSourceRules(code)
			if err != nil {
				logger.Warning("路由验证：构造地区限制匹配器失败，inbound id:", in.Id, "err:", err)
			}
			byCode[code] = rules // 失败时存 nil，同一个代码的入站不再重复尝试
		}
		if rules != nil {
			g.byInbound[in.Id] = rules
		}
	}
	return g
}

func buildGeoSourceRules(tag string) (*geoSourceRules, error) {
	xray.EnsureAssetLocation()
	v6, outside := geoRuleSources(tag)
	build := func(src []string) (*router.IPMatcher, error) {
		rules, err := geodata.ParseIPRules(src)
		if err != nil {
			return nil, err
		}
		return router.NewIPMatcher(rules, router.MatcherAsType_Source)
	}
	r := &geoSourceRules{}
	var err error
	if r.v6, err = build(v6); err != nil {
		return nil, err
	}
	if r.outside, err = build(outside); err != nil {
		return nil, err
	}
	return r, nil
}

// Blocks 报告来自 sourceIP、进入 inboundId 的连接会不会被该入站的地区规则拒绝。
// 入站没配地区限制、来源不是合法 IP 时一律为否。nil 接收者安全。
func (g *geoBlockMatcher) Blocks(inboundId int, sourceIP string) bool {
	if g == nil {
		return false
	}
	ip := net.ParseIP(sourceIP)
	if ip == nil {
		return false
	}
	if g.closed[inboundId] {
		return true
	}
	if stored := g.storedAllow[inboundId]; stored != nil {
		// 存储的地区之外（IPv6 一律在外）：窗口内某个时刻必然会被地区规则拦下。
		if ip.To4() == nil || !stored.Contains(sourceIP) {
			return true
		}
	}
	rules := g.byInbound[inboundId]
	if rules == nil {
		return false
	}
	// IPv4 必须按 4 字节传：16 字节的 v4 映射地址有被 "::/0" 当成 IPv6 命中的风险，
	// 而核心收到的真实来源是 4 字节的。
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	ctx := routercmd.AsRoutingContext(&routercmd.RoutingContext{SourceIPs: [][]byte{ip}})
	return rules.v6.Apply(ctx) || rules.outside.Apply(ctx)
}

// isGeoBlocked 是访问日志一行的地区拦截判定：只有确实进了黑洞、且来源被地区规则拒绝的才算。
func (g *geoBlockMatcher) isGeoBlocked(inboundId int, route, sourceIP string) bool {
	return routeHead(route) == model.BlockOutboundTag && g.Blocks(inboundId, sourceIP)
}
