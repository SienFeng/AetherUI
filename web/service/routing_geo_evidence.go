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
// 已知局限：xray 的 geoip 匹配器按「文件:代码」做弱引用缓存。IP 归属地库更新、dat 内容
// 变了而 tag 不变时，若旧匹配器仍被别处引用，这里可能命中旧集合，在允许集边缘的个别 IP
// 上判断滞后一轮。dat 变化会改 ruleTag 里的哈希、触发配置变更与代次递增，异常扫描的计划
// 随之重建，旧匹配器不再被引用后即可回收。
type geoBlockMatcher struct {
	// byInbound 是配了地区限制的启用入站 → 它那两条拒绝规则的 source 匹配器。
	byInbound map[int]*geoSourceRules
}

type geoSourceRules struct {
	v6, outside *router.IPMatcher
}

// newGeoBlockMatcher 为配了地区限制的启用入站构建匹配器。某个入站的地区数据读不出、
// 匹配器建不出（例如 dat 还没生成）时只跳过那个入站并记日志：那个入站的封禁连接退回
// 原来的口径（计入「走了其他出站」），宁可多报也不把真正走错的连接藏起来。
func newGeoBlockMatcher(inbounds []*model.Inbound) *geoBlockMatcher {
	g := &geoBlockMatcher{byInbound: map[int]*geoSourceRules{}}
	byTag := map[string]*geoSourceRules{}
	for _, in := range inbounds {
		if !in.Enable {
			continue // 停用入站不在核心配置里，也没有它的地区规则（buildGeoPlan 同样跳过）
		}
		provinces, err := DecodeRegions(in.Regions)
		if err != nil || len(provinces) == 0 {
			continue
		}
		tag := regionTag(provinces)
		rules, ok := byTag[tag]
		if !ok {
			rules, err = buildGeoSourceRules(tag)
			if err != nil {
				logger.Warning("路由验证：构造地区限制匹配器失败，inbound id:", in.Id, "err:", err)
			}
			byTag[tag] = rules // 失败时存 nil，同 tag 的入站不再重复尝试
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
	rules := g.byInbound[inboundId]
	if rules == nil {
		return false
	}
	ip := net.ParseIP(sourceIP)
	if ip == nil {
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
