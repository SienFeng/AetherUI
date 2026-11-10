package service

import (
	"bytes"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/netdiag"
)

// selectOverQuota 按「先来后到」挑出超出额度、应当被断开的来源 IP。
//
// limit <= 0 表示不限制。保留最早上线的 limit 个，其余全部拒绝——这是
// 与管理员确认过的语义：拒绝后来的，不动已经在线的人。
//
// 次序必须是确定的：面板刚重启时所有 IP 的首次观测时间相同，若此时次序
// 随机，每一轮踢掉的人都不一样，结果是谁都用不成。因此首次观测时间相同
// 时按 IP 字节升序做二次排序。
func selectOverQuota(list []OnlineIP, limit int) []string {
	if limit <= 0 || len(list) <= limit {
		return nil
	}

	sorted := make([]OnlineIP, len(list))
	copy(sorted, list)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].FirstSeen != sorted[j].FirstSeen {
			return sorted[i].FirstSeen < sorted[j].FirstSeen
		}
		return bytes.Compare(
			net.ParseIP(sorted[i].IP).To16(),
			net.ParseIP(sorted[j].IP).To16(),
		) < 0
	})

	over := make([]string, 0, len(sorted)-limit)
	for _, e := range sorted[limit:] {
		over = append(over, e.IP)
	}
	return over
}

// 上一轮各入站被拒的 IP 集合，只用于决定要不要写日志：
// 判定每秒跑一次，不去重的话一个反复重连的客户端会把日志刷爆。
var (
	concurrencyLogLock sync.Mutex
	concurrencyLogged  = map[int]map[string]bool{}
)

type ConcurrencyService struct {
	inboundService InboundService
	onlineService  OnlineService
	settingService SettingService
	banService     IPBanService
}

// limitedInbounds 返回真正需要做并发判定的入站，见 limitedInboundsWithDefault。
func (s *ConcurrencyService) limitedInbounds(now time.Time) ([]*model.Inbound, error) {
	inbounds, _, err := s.limitedInboundsWithDefault(now)
	return inbounds, err
}

// limitedInboundsWithDefault 返回需要做并发判定的入站与默认的同时在用网络数：启用的入站里，自己设了额度的、
// 有封禁的，以及默认额度不为 0 时的全部。停用的入站根本不在 xray 配置里，没有连接可管。
func (s *ConcurrencyService) limitedInboundsWithDefault(now time.Time) ([]*model.Inbound, int, error) {
	def := s.settingService.GetDefaultConcurrencyLimit()
	var inbounds []*model.Inbound
	// 有封禁的入站也要进判定，否则没设并发额度的入站上封禁永远不会被执行。
	banned := database.GetDB().Model(&model.IPBan{}).
		Select("inbound_id").
		Where("expires_at = 0 or expires_at > ?", now.UnixMilli())
	q := database.GetDB().Model(model.Inbound{}).Where("enable = ?", true)
	if def <= 0 {
		q = q.Where("concurrency_limit > 0 or id in (?)", banned)
	}
	if err := q.Order("id asc").Find(&inbounds).Error; err != nil {
		return nil, 0, err
	}
	return inbounds, def, nil
}

// effectiveConcurrencyLimit：入站自己设了额度用自己的，没设（0）用默认的同时在用网络数。
func effectiveConcurrencyLimit(in *model.Inbound, def int) int {
	if in.ConcurrencyLimit > 0 {
		return in.ConcurrencyLimit
	}
	return max(def, 0)
}

// networkKey 是「一个网络」的身份：IPv4 一个地址（同一个 Wi-Fi 的设备共用一个出口地址），IPv6 按 /64
// （同一户人家的设备、手机轮换的隐私地址都在同一个 /64 里——按单个地址数，一台手机就能占满额度）。
func networkKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// planNetworkRejections 决定某入站本轮应当拒绝哪些来源 IP：按网络（networkKey）数同时在用的来源，
// 先保留本人常住省（home 为真）的网络，同一档里先来后到保留最早的，凑满 limit 个网络，其余网络的全部 IP 拒绝。
//
// blocked 为真的来源不计入：被地区规则、机房拦截挡在门外的连接本来就用不了这个入站，让它占名额的话，
// 额度为 1 时一个先连上、一直被拦着重试的外省共享者会把本人挤掉。
//
// home 为 nil 时只按先来后到（见 homeNetworks：只在持有通行证的入站上才有）。不按 home 排序的话，名额归
// 连接一直没断的那一方：外省通行证生效期间本人回到家里，只要通行证省份那边一直连着，本人每秒被断一次，
// 要等自动出差放行看到「家里恢复使用」收回通行证（家里攒够 1 MB 之后最多再一分多钟），这段时间只能断续使用。
//
// 家里的来源要**真的在用**才排到前面：本次在线已传过 coexistMinActiveBytes，与 liveSignals 判「家里在用」
// 同一口径。在线表对第一次见到的 IP 一律记为活跃、也不看鉴权，不设门槛的话，出差中的本人会被家里省一条
// 几百字节的连接（设备客户端的测速、主动探测）挤掉；有了门槛，被挤掉的时刻恰好是自动出差放行也会认定
// 「家里恢复使用」、收回通行证的时刻，两边结论一致。
func planNetworkRejections(list []OnlineIP, limit int, blocked, home func(ip string) bool) []string {
	if limit <= 0 {
		return nil
	}
	type network struct {
		key       string
		firstSeen int64
		home      bool
		ips       []string
	}
	byKey := map[string]*network{}
	var nets []*network
	for _, e := range liveOnly(list) {
		if blocked != nil && blocked(e.IP) {
			continue
		}
		k := networkKey(e.IP)
		n := byKey[k]
		if n == nil {
			n = &network{key: k, firstSeen: e.FirstSeen}
			byKey[k] = n
			nets = append(nets, n)
		}
		n.firstSeen = min(n.firstSeen, e.FirstSeen)
		n.home = n.home || (home != nil && e.Up+e.Down >= coexistMinActiveBytes && home(e.IP))
		n.ips = append(n.ips, e.IP)
	}
	if len(nets) <= limit {
		return nil
	}
	// 次序必须确定（理由见 selectOverQuota）：首次观测时间相同时按网络身份排序。
	sort.Slice(nets, func(i, j int) bool {
		if nets[i].home != nets[j].home {
			return nets[i].home
		}
		if nets[i].firstSeen != nets[j].firstSeen {
			return nets[i].firstSeen < nets[j].firstSeen
		}
		return nets[i].key < nets[j].key
	})
	var over []string
	for _, n := range nets[limit:] {
		over = append(over, n.ips...)
	}
	sort.Strings(over)
	return over
}

var (
	concurrencyBlockMu    sync.Mutex
	concurrencyBlockAt    time.Time
	concurrencyBlockGeo   *geoBlockMatcher
	concurrencyBlockCount int
	// concurrencyHome 是持有通行证的入站 → 它**存储的**地区（本人常住省）的允许集，与 concurrencyBlockGeo
	// 同一时刻重建。只放建得出来的：IP 库没加载时这个入站退回只按先来后到。
	concurrencyHome map[int]*cidrSet
)

// concurrencyHomeSetOf 构造存储地区的允许集；测试里替换，免得依赖真实 IP 库。
var concurrencyHomeSetOf = allowedSetFor

// homeNetworks 构造持有通行证的入站的「本人常住省」允许集，按存储的地区（管理员设的或自动设置的那个省）。
//
// 只给持有通行证的入站：没有通行证时，存储地区之外的来源已经被核心挡在门外、不占名额（sourceBlocker），
// 剩下的全在家里省，再排一次序不改变任何结果；而允许集每 30 秒建一次，不值得为它们付这份开销。
// 同一组省份只建一次。
func homeNetworks(inbounds []*model.Inbound, passes map[int]*model.InboundRegionPass) map[int]*cidrSet {
	out := map[int]*cidrSet{}
	byKey := map[string]*cidrSet{}
	for _, in := range inbounds {
		if passes[in.Id] == nil {
			continue
		}
		stored, err := DecodeRegions(in.Regions)
		if err != nil || len(stored) == 0 {
			continue
		}
		key := strings.Join(stored, ",")
		set, ok := byKey[key]
		if !ok {
			set, err = concurrencyHomeSetOf(stored)
			if err != nil {
				set = nil
			}
			byKey[key] = set
		}
		if set != nil {
			out[in.Id] = set
		}
	}
	return out
}

// homeFor 返回某入站的「来源在本人常住省」判断；没有就返回 nil（只按先来后到）。
func (s *ConcurrencyService) homeFor(in *model.Inbound) func(ip string) bool {
	concurrencyBlockMu.Lock()
	set := concurrencyHome[in.Id]
	concurrencyBlockMu.Unlock()
	if set == nil {
		return nil
	}
	return set.Contains
}

// sourceBlocker 返回「这个来源会被核心挡在门外」的判断：地区规则（按核心用上的那一份）与机房拦截。
// 匹配器 30 秒重建一次——判定每秒一轮，每轮读 dat、建集合太贵；通行证的增减最多晚 30 秒反映到这里。
func (s *ConcurrencyService) sourceBlocker(inbounds []*model.Inbound, now time.Time) func(in *model.Inbound) func(ip string) bool {
	concurrencyBlockMu.Lock()
	if concurrencyBlockGeo == nil || now.Sub(concurrencyBlockAt) > 30*time.Second || concurrencyBlockCount != len(inbounds) {
		// 只看核心此刻生效的规则（不回放存储地区）：持有通行证的入站，通行证省份的网络照样占名额。
		passes := activePassesOrNil()
		concurrencyBlockGeo = newLiveGeoBlockMatcher(inbounds, passes)
		concurrencyHome = homeNetworks(inbounds, passes)
		concurrencyBlockAt, concurrencyBlockCount = now, len(inbounds)
	}
	geo := concurrencyBlockGeo
	concurrencyBlockMu.Unlock()
	idc := idcBlockedSet(&s.settingService, &IPDBService{})
	return func(in *model.Inbound) func(ip string) bool {
		// 豁免机房拦截的入站（idcExempt）：机房来源真的连得进来，照常占名额——当成被拦的话，中转机
		// 永远不占名额，这个入站的并发限制对它们整个失效。
		exempt := idcExempt(in)
		return func(ip string) bool {
			if geo.Blocks(in.Id, ip) {
				return true
			}
			return idc != nil && !exempt && idc.Contains(ip)
		}
	}
}

// markBanned 给列表打上封禁标记，并把连接已经被断干净、因而不在连接表里的
// 封禁来源补进来——不补的话管理员在界面上根本看不到自己封了谁，也就无从解封。
func markBanned(list []OnlineIP, banned map[string]*model.IPBan) []OnlineIP {
	seen := make(map[string]bool, len(list))
	for i := range list {
		seen[list[i].IP] = true
		if b, ok := banned[list[i].IP]; ok {
			list[i].Banned = true
			list[i].BanExpiresAt = b.ExpiresAt
		}
	}
	for ip, b := range banned {
		if seen[ip] {
			continue
		}
		list = append(list, OnlineIP{IP: ip, Banned: true, BanExpiresAt: b.ExpiresAt})
	}
	sort.Slice(list, func(i, j int) bool {
		return bytes.Compare(net.ParseIP(list[i].IP).To16(), net.ParseIP(list[j].IP).To16()) < 0
	})
	return list
}

// Enforce 跑一轮并发判定。
//
// 它是**幂等收敛**的：每轮重新计算超额集合并对其执行断开，不依赖任何
// 跳变事件。因此漏掉一轮、面板重启、客户端重连都不会让状态跑偏。
func (s *ConcurrencyService) Enforce() error {
	now := time.Now()
	inbounds, def, err := s.limitedInboundsWithDefault(now)
	if err != nil {
		return err
	}
	if len(inbounds) == 0 {
		// 没人设额度、也没有封禁，就一次系统调用都不做，空转成本为零。
		// 但上一轮残留的被拒标记要清掉，见 retainRejectedPorts。
		onlineTrackerInstance.retainRejectedPorts(nil)
		return nil
	}
	if !netdiag.Supported {
		return netdiag.ErrUnsupported
	}
	if err := s.onlineService.sample(); err != nil {
		return err
	}

	// 读不到配置时退回 0（关闭闲置判定），保持改动前的行为：宁可额度释放
	// 得慢，也不要因为读配置失败就把所有人都当成闲置、并发限制整个失效。
	idleAfter := idleTimeoutOrZero(s.settingService.GetConcurrencyIdleTimeout())

	blockerFor := s.sourceBlocker(inbounds, now)
	enforced := make(map[int]bool, len(inbounds))
	defer func() { onlineTrackerInstance.retainRejectedPorts(enforced) }()
	for _, inbound := range inbounds {
		enforced[inbound.Port] = true
		list := onlineTrackerInstance.snapshotIdle(inbound.Port, noLocate, idleAfter)

		// 封禁先于额度判定执行：被封的连接每轮都要断，而且断完之后它腾出来的
		// 名额应当让给别人，所以它不能参与额度计算（liveOnly 已排除 Banned）。
		bans, err := s.banService.ActiveBans(inbound.Id, now)
		if err != nil {
			logger.Warning("读取封禁名单失败, 入站", inbound.Id, ":", err)
		} else if len(bans) > 0 {
			list = markBanned(list, bannedIPSet(bans))
			s.disconnectBanned(inbound, list)
		}

		over := planNetworkRejections(list, effectiveConcurrencyLimit(inbound, def), blockerFor(inbound), s.homeFor(inbound))
		onlineTrackerInstance.setRejected(inbound.Port, over, now)
		s.disconnect(inbound, over)
	}
	return nil
}

// disconnectBanned 断开封禁名单里仍有连接的来源。
//
// 没有连接的封禁来源直接跳过：Kick 会重新 dump 一次连接表，为一个早就断干净
// 的 IP 每秒来一趟内核调用没有意义。
func (s *ConcurrencyService) disconnectBanned(inbound *model.Inbound, list []OnlineIP) {
	for _, e := range list {
		if !e.Banned || e.Conns == 0 {
			continue
		}
		killed, err := s.onlineService.Kick(inbound.Id, e.IP)
		if err != nil {
			logger.Warning("封禁断开失败, 入站", inbound.Id, "IP", e.IP, ":", err)
			continue
		}
		if killed > 0 {
			logger.Warning("封禁生效: 入站", inbound.Id, "IP", e.IP, "断开", killed, "条连接")
		}
	}
}

// noLocate 用于判定路径：这里不需要归属地与运营商，省掉每轮的查库开销。
func noLocate(net.IP) ipLocation { return ipLocation{} }

// planRejections 决定某入站本轮应当拒绝哪些来源 IP：先剔除已断连的历史
// 条目，再按先来后到挑出超额的部分。两步必须按这个顺序，理由见 liveOnly。
func planRejections(list []OnlineIP, limit int) []string {
	return selectOverQuota(liveOnly(list), limit)
}

// liveOnly 剔除"已被拒、连接已断"的历史条目。
//
// 这些条目只为界面展示而存在。把它们算进在线数会让被拒的 IP 永久占着一个
// 名额：额度 1 时第一个被拒的人会把所有人（包括他自己）永远挡在门外，
// 而且这个状态在保留期内自愈不了。
// liveOnly 挑出真正占用并发额度的来源。
//
// 闲置来源被排除在外，两个方向都成立：它不占别人的额度，自己也不会被判超额
// 而遭断开——断一个只是暂时没有流量的正常用户毫无意义。
//
// 被封禁的来源同样排除：它的连接每轮都会被断开，让它占着名额等于把额度
// 白白吃掉。
func liveOnly(list []OnlineIP) []OnlineIP {
	live := make([]OnlineIP, 0, len(list))
	for _, e := range list {
		if e.Conns > 0 && !e.Idle && !e.Banned {
			live = append(live, e)
		}
	}
	return live
}

func (s *ConcurrencyService) disconnect(inbound *model.Inbound, over []string) {
	logged := takeLoggedSet(inbound.Id, over)
	for _, ip := range over {
		killed, err := s.onlineService.Kick(inbound.Id, ip)
		if err != nil {
			logger.Warning("并发超额断开失败, 入站", inbound.Id, "IP", ip, ":", err)
			continue
		}
		// 只在这个 IP 是本轮新被拒时记一条，避免反复重连刷日志。
		if !logged[ip] && killed > 0 {
			logger.Warning("并发超额: 入站", inbound.Id, "额度", inbound.ConcurrencyLimit,
				"拒绝来源 IP", ip, "断开", killed, "条连接")
		}
	}
}

// takeLoggedSet 返回上一轮该入站已记过日志的 IP 集合，并把本轮集合存下来。
func takeLoggedSet(inboundId int, over []string) map[string]bool {
	concurrencyLogLock.Lock()
	defer concurrencyLogLock.Unlock()

	prev := concurrencyLogged[inboundId]
	if prev == nil {
		prev = map[string]bool{}
	}
	next := make(map[string]bool, len(over))
	for _, ip := range over {
		next[ip] = true
	}
	concurrencyLogged[inboundId] = next
	return prev
}

// idleTimeoutOrZero 把设置项的秒数折算成 Duration，出错时返回 0（关闭判定）。
func idleTimeoutOrZero(seconds int, err error) time.Duration {
	if err != nil {
		logger.Warning("读取并发闲置超时失败，本轮不做闲置判定:", err)
		return 0
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
