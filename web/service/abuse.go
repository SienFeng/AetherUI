package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/ipdb"
)

// 防滥用：机房来源、25 端口发信、流量识别补全。三项都是「面板设置 → 防共享 / 防滥用」里的全局开关，
// 管理员 2026-11-09 选定默认打开（用户都是直连，没有中转与 CDN）。

const (
	idcDatPath = "bin/a-ui-idc.dat"
	idcDatRef  = "a-ui-idc.dat"
	// idcDatKeep 是 dat 里最多留几份代码：本轮的加上最近写过的。按内容寻址之后，IP 库一更新代码就变，
	// 正在运行的配置引用的旧代码不能被新文件挤掉（与 a-ui-geo.dat 同一个理由，见 writeGeoDatRetaining）。
	idcDatKeep = 3
)

var (
	idcMu sync.Mutex
	// idcFor 是算出 idcCIDRs 的那份 IP 库：IP 库换了（更新、加载）才重算，多数票扫描要走一遍全部源的全部网段。
	// 比较用 SameSources 而不是指针相等：IPDBService.DB() 每次都新建 Multi 包装。
	idcFor   *ipdb.Multi
	idcCIDRs []string
	idcSet   *cidrSet
	// idcRecent 是最近写进 dat 的代码，新的在前。
	idcRecent []string
	// idcWriteFailLogged：写 dat 失败的告警 10 分钟一条（每次生成配置都会走到这里）。
	idcWriteFailLogged time.Time
)

// resetIDCState 只在测试之间清状态。
func resetIDCState(t interface{ Helper() }) {
	t.Helper()
	idcMu.Lock()
	defer idcMu.Unlock()
	idcFor, idcCIDRs, idcSet, idcRecent = nil, nil, nil, nil
}

// idcSnapshot 返回这份 IP 库认定的机房网段（多数票，见 ipdb.Multi.MajorityCIDRsOfIDC）与对应的集合。
func idcSnapshot(db *ipdb.Multi) ([]string, *cidrSet) {
	if db == nil {
		return nil, nil
	}
	idcMu.Lock()
	defer idcMu.Unlock()
	if !idcFor.SameSources(db) {
		idcFor = db
		idcCIDRs = db.MajorityCIDRsOfIDC()
		idcSet = newCIDRSet(idcCIDRs)
	}
	return idcCIDRs, idcSet
}

func idcDatCode(cidrs []string) string {
	sum := sha256.Sum256([]byte(strings.Join(cidrs, "\n")))
	return "IDCC" + strings.ToUpper(hex.EncodeToString(sum[:4]))
}

// ensureIDCDat 把机房网段写进 a-ui-idc.dat（本轮的代码在前，再留最近写过的），返回本轮的代码。
func ensureIDCDat(cidrs []string) (string, error) {
	code := idcDatCode(cidrs)
	idcMu.Lock()
	defer idcMu.Unlock()
	if len(idcRecent) == 0 {
		// 面板刚启动：文件里已有的代码可能正被运行中的核心引用，先把它们当成「最近写过的」。
		if entries, _, err := readGeoDat(idcDatPath); err == nil {
			for _, e := range entries {
				idcRecent = append(idcRecent, e.Tag)
			}
		}
	}
	keep := []string{code}
	for _, c := range idcRecent {
		if c != code && len(keep) < idcDatKeep {
			keep = append(keep, c)
		}
	}
	existing := map[string][]string{code: cidrs}
	if len(keep) > 1 {
		if entries, _, err := readGeoDat(idcDatPath); err == nil {
			for _, e := range entries {
				existing[e.Tag] = e.CIDRs
			}
		}
	}
	entries := make([]geodatEntry, 0, len(keep))
	for _, c := range keep {
		if list, ok := existing[c]; ok {
			entries = append(entries, geodatEntry{Tag: c, CIDRs: list})
		}
	}
	if _, err := writeGeoDat(idcDatPath, entries); err != nil {
		return "", err
	}
	idcRecent = keep
	return code, nil
}

// abuseRuleSet 是防滥用规则的全部输入（纯数据）。
type abuseRuleSet struct {
	Inbounds  []*model.Inbound
	BlockSmtp bool
	// IDCCode 是机房网段在 a-ui-idc.dat 里的代码，空表示不拦机房来源。
	IDCCode string
}

// abuseRules 生成防滥用的拦截规则。只作用于启用的、非 dokodemo-door 的入站：端口转发可能本来就来自
// 服务器、也可能本来就是在转发邮件端口，那是管理员的明确配置。按入站 id 排序，逐字节确定。
func abuseRules(in abuseRuleSet) []any {
	if !in.BlockSmtp && in.IDCCode == "" {
		return nil
	}
	targets := make([]*model.Inbound, 0, len(in.Inbounds))
	for _, ib := range in.Inbounds {
		if ib.Enable && !strings.EqualFold(string(ib.Protocol), "dokodemo-door") && ib.Tag != "" {
			targets = append(targets, ib)
		}
	}
	// 空的 inboundTag 在 xray 里等于「所有入站」，包括模板里的 api 入站。
	if len(targets) == 0 {
		return nil
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Id < targets[j].Id })
	tags := make([]string, 0, len(targets))
	for _, ib := range targets {
		tags = append(tags, ib.Tag)
	}
	var rules []any
	if in.BlockSmtp {
		rules = append(rules, map[string]any{
			"type": "field", "ruleTag": "a-ui-smtp", "inboundTag": tags,
			"port": "25", "network": "tcp", "outboundTag": model.BlockOutboundTag,
		})
	}
	if in.IDCCode != "" {
		rules = append(rules, map[string]any{
			"type": "field", "ruleTag": "a-ui-idc", "inboundTag": tags,
			"source": []string{"ext:" + idcDatRef + ":" + in.IDCCode}, "outboundTag": model.BlockOutboundTag,
		})
	}
	return rules
}

// buildAbuseRules 读设置、准备机房网段的 dat，生成防滥用规则。机房 dat 写不进去时不拦机房来源（只告警）：
// 引用一个文件里没有的代码，xray 会整个起不来。
func (s *RoutingInjector) buildAbuseRules(inbounds []*model.Inbound) []any {
	set := abuseRuleSet{Inbounds: inbounds, BlockSmtp: s.settingService.GetBlockSmtp()}
	if s.settingService.GetBlockIdcSources() {
		if cidrs, _ := idcSnapshot(s.ipdbService.DB()); len(cidrs) > 0 {
			code, err := ensureIDCDat(cidrs)
			if err != nil {
				idcMu.Lock()
				if time.Since(idcWriteFailLogged) > 10*time.Minute {
					idcWriteFailLogged = time.Now()
					logger.Warning("写入机房网段数据文件失败，本轮不拦机房来源:", err)
				}
				idcMu.Unlock()
			} else {
				set.IDCCode = code
			}
		}
	}
	return abuseRules(set)
}

// idcBlockedSet 返回「拦机房来源」开着时的机房网段集合（给并发判定与自动出差剔除被拦的来源用），关着返回 nil。
func idcBlockedSet(settings *SettingService, ipdbService *IPDBService) *cidrSet {
	if !settings.GetBlockIdcSources() {
		return nil
	}
	_, set := idcSnapshot(ipdbService.DB())
	return set
}

// forcedSniffing 补全一个入站的流量识别（「补全流量识别」开关）：
//   - 关着嗅探的：打开只用于分流的嗅探（routeOnly，不改连接目标）——BT 拦截与域名规则才看得见这些连接；
//   - 开着但没识别 QUIC 的：补上 quic——否则 HTTP/3（UDP 443）只露出 IP，域名封禁与分流规则全部绕过。
//
// 只对支持嗅探的协议（与 xray.js 的 canSniffing 同口径）；解析不了的原样返回。结果逐字节确定。
func forcedSniffing(protocol, raw string) string {
	switch strings.ToLower(protocol) {
	case "vmess", "vless", "trojan", "shadowsocks", "socks", "http":
	default:
		return raw
	}
	sniff := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &sniff); err != nil {
			return raw
		}
	}
	if enabled, _ := sniff["enabled"].(bool); !enabled {
		out, _ := json.Marshal(map[string]any{
			"enabled": true, "destOverride": []string{"http", "tls", "quic"}, "routeOnly": true,
		})
		return string(out)
	}
	overrides, _ := sniff["destOverride"].([]any)
	for _, o := range overrides {
		if o == "quic" {
			return raw
		}
	}
	sniff["destOverride"] = append(overrides, "quic")
	out, err := json.Marshal(sniff)
	if err != nil {
		return raw
	}
	return string(out)
}
