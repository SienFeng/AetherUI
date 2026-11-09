package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/geodat"
	"a-ui/util/ipdb"
)

// geoDatPath 与 bin/config.json 一样是相对路径。
//
// geoDatRef 是配置里 ext: 引用时用的名字：xray 解析 ext 文件是按「可执行文件
// 所在目录」找的，而 xray 二进制就在 bin/ 下，所以引用时不带目录前缀。
// 这一点已用真实 xray 实测确认。
const (
	geoDatPath = "bin/a-ui-geo.dat"
	geoDatRef  = "a-ui-geo.dat"
)

// geodatEntry 是 geodat.Entry 的别名，避免调用方到处 import geodat。
type geodatEntry = geodat.Entry

// EncodeRegions 把地区列表编码成库里存的 JSON 字符串。
// 排序去重后再编码：省份集合决定 dat 里的 tag，顺序不定会让同一个集合
// 生成两个 tag，dat 里出现重复数据。
func EncodeRegions(list []string) (string, error) {
	b, err := json.Marshal(normalizeRegions(list))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// EncodeRegionsStrict 在「用户选了地区、却被全部过滤成空」时报错。
//
// 与入站选择是同一类事故：空列表表示不限制，静默变成不限制的话，
// 界面显示他选过江苏，实际全世界都能连。
func EncodeRegionsStrict(list []string) (string, error) {
	encoded, err := EncodeRegions(list)
	if err != nil {
		return "", err
	}
	if len(list) > 0 && encoded == "[]" {
		return "", common.NewError("地区选择非法：提交了", len(list), "项，但没有一个是有效的地区名")
	}
	return encoded, nil
}

// DecodeRegions 是 EncodeRegions 的逆操作，返回排序去重后的列表。
//
// 空字符串与 "null" 当作「不限制」：老数据没有这个字段，在这里报错会让
// 整份配置生成失败。真正的语法错误仍返回 error——数据损坏时宁可让配置
// 生成失败，也不能静默退化成「不限制」。
func DecodeRegions(encoded string) ([]string, error) {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
		return nil, err
	}
	return normalizeRegions(list), nil
}

func itoa(i int) string { return strconv.Itoa(i) }

func normalizeRegions(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, r := range list {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// regionTag 由省份集合算出 dat 里的 tag。
//
// 用哈希而不是拼省份名：dat 的 tag 查找大小写不敏感，而且中文名进配置
// 字符串会引入编码上的不确定性。纯大写 ASCII 最稳。
func regionTag(provinces []string) string {
	sum := sha256.Sum256([]byte(strings.Join(normalizeRegions(provinces), "\x00")))
	return "ALLOW" + strings.ToUpper(hex.EncodeToString(sum[:4]))
}

// 地区限制的两种合并口径。
const (
	// RegionMatchUnion 是并集：任一源认为该段属于所选省份就放行。**默认值。**
	//
	// 保持默认是因为实测差距没有想象中大——三源并集只比最大的单源宽 1%~18%
	//（按覆盖的 IP 地址数），而这条路径生成的是允许集，收紧的代价是正常用户
	// 连不上，那是即时可见的投诉；放宽的代价是地区限制被稀释，慢性且可察觉。
	// util/ipdb/multi.go 那句「本该能连却连不上比本不该连却连上了严重」在这个
	// 量级下依然成立。
	RegionMatchUnion = 0
	// RegionMatchMajority 是多数判据：支持数必须严格大于反对数，沉默不计入。
	// 判据细节见 ipdb.Multi.MajorityCIDRsOfProvinces。
	RegionMatchMajority = 1
)

// provinceCIDRSource 是 buildGeoPlan 需要的全部能力，方便测试替换。
type provinceCIDRSource interface {
	CIDRsOfProvinces(provinces []string) []string
}

// majorityCIDRSource 把 Multi 的多数判据包装成 provinceCIDRSource。
//
// 做成适配器而不是给 buildGeoPlan 加一个模式参数：那样每个调用点和每条测试
// 都要跟着改，而它们关心的只是「给我这些省份的 IP 段」，口径由上层选定。
type majorityCIDRSource struct{ m *ipdb.Multi }

func (s majorityCIDRSource) CIDRsOfProvinces(provinces []string) []string {
	return s.m.MajorityCIDRsOfProvinces(provinces)
}

// regionCIDRSource 按面板设置选出地区限制的合并口径。
//
// 取不到设置、或设置是默认值时一律退回并集：这条路径生成的是**允许集**，
// 而空的或过窄的允许集配上 ! 取反就是把人挡在门外。读设置失败是个与地区
// 限制无关的故障（库锁、磁盘满），不该由它决定谁能连上。
func regionCIDRSource(db *ipdb.Multi, settingService *SettingService) provinceCIDRSource {
	mode, err := settingService.GetRegionMatchMode()
	if err != nil {
		logger.Warning("读取地区匹配模式失败，本轮按并集口径生成:", err)
		return db
	}
	if mode == RegionMatchMajority {
		return majorityCIDRSource{db}
	}
	return db
}

// geoPlan 是一轮生成的结果：dat 里要写哪几组数据，以及每个入站用哪个 tag。
type geoPlan struct {
	Entries      []geodatEntry
	TagByInbound map[int]string
	// FailClosed 是本轮只能锁死的入站（id → 原因），它们不引用 dat。
	FailClosed map[int]string
	// Degraded 是本轮降级的入站，含沿用旧允许集（stale）与锁死（closed）两种。
	Degraded map[int]GeoDegradation
}

// buildGeoPlan 是 buildGeoPlanWith 的严格包装：任一入站需要降级就返回错误。
// 只给既有的单元测试与「保存时预检」用；生成配置走 buildGeoPlanWith。
func buildGeoPlan(inbounds []*model.Inbound, db provinceCIDRSource) (*geoPlan, error) {
	plan := buildGeoPlanWith(inbounds, db, nil, nil)
	for _, in := range inbounds {
		if reason, ok := plan.FailClosed[in.Id]; ok {
			return nil, common.NewError(reason)
		}
	}
	return plan, nil
}

// buildGeoPlanWith 按省份集合去重，算出 dat 的内容与每个入站对应的 tag。
//
// 不返回错误：出问题的入站沿用 lastGood 里同 tag 的允许集，没有就锁死，其余入站
// 照常生成（理由见 geo_degrade.go）。db 为 nil 表示 IP 归属地库未加载。
// passes 是活动出差通行证（入站 id → 通行证），按 effectiveRegions 并入。
func buildGeoPlanWith(inbounds []*model.Inbound, db provinceCIDRSource, passes map[int]*model.InboundRegionPass, lastGood map[string][]string) *geoPlan {
	plan := &geoPlan{
		TagByInbound: map[int]string{},
		FailClosed:   map[int]string{},
		Degraded:     map[int]GeoDegradation{},
	}
	byTag := map[string][]string{}
	// failedTag 记住本轮已判定为「算不出」的 tag，同 tag 的入站沿用同一结论。
	failedTag := map[string]string{}

	closeInbound := func(id int, reason string) {
		plan.FailClosed[id] = reason
		plan.Degraded[id] = GeoDegradation{Mode: geoDegradeClosed, Reason: reason}
	}

	for _, in := range inbounds {
		if !in.Enable {
			continue
		}
		provinces, suspended, err := effectiveRegions(in, passes[in.Id])
		if err != nil {
			closeInbound(in.Id, fmt.Sprint("入站的地区数据损坏: ", err))
			continue
		}
		if suspended || len(provinces) == 0 {
			continue
		}
		tag := regionTag(provinces)
		if reason, failed := failedTag[tag]; failed {
			if cidrs, ok := lastGood[tag]; ok && len(cidrs) > 0 {
				plan.TagByInbound[in.Id] = tag
				plan.Degraded[in.Id] = GeoDegradation{Mode: geoDegradeStale, Reason: reason}
			} else {
				closeInbound(in.Id, reason)
			}
			continue
		}
		if _, ok := byTag[tag]; ok {
			plan.TagByInbound[in.Id] = tag
			continue
		}
		var cidrs []string
		reason := ""
		if db == nil {
			reason = "IP 归属地库未加载"
		} else if cidrs = db.CIDRsOfProvinces(provinces); len(cidrs) == 0 {
			reason = "找不到这些地区的 IP 段: " + strings.Join(provinces, "、") +
				"（IP 归属地库可能未加载或地区名已变更）"
		}
		if reason == "" {
			byTag[tag] = cidrs
			plan.TagByInbound[in.Id] = tag
			continue
		}
		failedTag[tag] = reason
		if old, ok := lastGood[tag]; ok && len(old) > 0 {
			byTag[tag] = old
			plan.TagByInbound[in.Id] = tag
			plan.Degraded[in.Id] = GeoDegradation{Mode: geoDegradeStale, Reason: reason}
			continue
		}
		closeInbound(in.Id, reason)
	}

	tags := make([]string, 0, len(byTag))
	for tag := range byTag {
		tags = append(tags, tag)
	}
	// 按 tag 升序：dat 内容必须逐字节确定，否则内容哈希每轮都变，
	// 那个 10 秒的重启 cron 会不停重启 xray。禁止遍历 map 产生顺序。
	sort.Strings(tags)
	for _, tag := range tags {
		plan.Entries = append(plan.Entries, geodatEntry{Tag: tag, CIDRs: byTag[tag]})
	}
	return plan
}

// buildGeoRules 为每个配了地区的入站生成两条拒绝规则。
//
// 顺序是两条都要、且 IPv6 那条在前：实测确认纯 IPv4 的允许集配上 ! 取反，
// 遇到 IPv6 来源会 **fail open**（放行），匹配器不会拿 v6 地址去比对一个
// 没有 v6 条目的集合。只靠把 listen 限定成 IPv4 也不够——管理员随时可能
// 把它改回留空，那时地区限制会静默失效。
func buildGeoRules(inbounds []*model.Inbound, db provinceCIDRSource, datHash string) ([]any, error) {
	plan, err := buildGeoPlan(inbounds, db)
	if err != nil {
		return nil, err
	}
	return rulesFromGeoPlan(inbounds, plan, datHash), nil
}

// geoRuleSources 是一个允许集 tag 对应的两条拒绝规则的 source 条件，顺序同规则顺序：
// 先拒绝全部 IPv6 来源，再拒绝不在允许集里的 IPv4 来源。
//
// 规则生成（rulesFromGeoPlan）与流量走向验证（geoBlockMatcher）共用这一份：验证要用
// xray 的匹配器按同样的条件判断「这条连接是不是被地区限制拦下的」，两边各写一份迟早漂移。
func geoRuleSources(tag string) (v6, outside []string) {
	return []string{"::/0"}, []string{"ext:" + geoDatRef + ":!" + tag}
}

func rulesFromGeoPlan(inbounds []*model.Inbound, plan *geoPlan, datHash string) []any {
	rules := make([]any, 0, len(plan.TagByInbound)*2+len(plan.FailClosed))
	// 按入站 id 升序遍历，不遍历 map。
	for _, in := range inbounds {
		if _, closed := plan.FailClosed[in.Id]; closed {
			// 不引用 dat：写 dat 失败时也必须能生成这一条。
			rules = append(rules, map[string]any{
				"type":        "field",
				"ruleTag":     "a-ui-geo-closed-" + itoa(in.Id),
				"inboundTag":  []string{in.Tag},
				"source":      []string{"0.0.0.0/0", "::/0"},
				"outboundTag": model.BlockOutboundTag,
			})
			continue
		}
		tag, ok := plan.TagByInbound[in.Id]
		if !ok {
			continue
		}
		v6, outside := geoRuleSources(tag)
		rules = append(rules, map[string]any{
			"type":        "field",
			"ruleTag":     "a-ui-geo6-" + itoa(in.Id),
			"inboundTag":  []string{in.Tag},
			"source":      v6,
			"outboundTag": model.BlockOutboundTag,
		})
		rules = append(rules, map[string]any{
			"type": "field",
			// dat 的内容变化不体现在配置字节里，Config.Equals 察觉不到，
			// xray 也就不会重新加载。把内容哈希嵌进 ruleTag，哈希变则配置
			// 字节变则重启，不必为 dat 单独造一套变更检测。
			"ruleTag":     "a-ui-geo-" + itoa(in.Id) + "-" + datHash,
			"inboundTag":  []string{in.Tag},
			"source":      outside,
			"outboundTag": model.BlockOutboundTag,
		})
	}
	return rules
}

// AnyInboundUsesRegions 判断是否有启用的入站配了地区限制。
// 没有的话整条地区路径完全不走：不碰 IP 库、不生成 dat，
// 这样没装归属地库的机器也能照常工作。
func AnyInboundUsesRegions(inbounds []*model.Inbound) bool {
	for _, in := range inbounds {
		if in.Enable && model.HasRegions(in.Regions) {
			return true
		}
	}
	return false
}

// RegionListResult 是「可选地区列表」接口的返回体。
type RegionListResult struct {
	// Loaded 为 false 时 List 一定为空，界面要提示去更新 IP 归属地库，
	// 而不是让管理员以为没有可选地区。
	Loaded bool     `json:"loaded"`
	List   []string `json:"list"`
}

type GeoService struct {
	ipdbService IPDBService
}

// Regions 返回可选的地区列表，来源就是 IP 归属地库里的省级名称。
func (s *GeoService) Regions() *RegionListResult {
	db := s.ipdbService.DB()
	if db == nil {
		// 不能给 null：前端直接拿去渲染下拉框。
		return &RegionListResult{List: []string{}}
	}
	list := db.Provinces()
	if list == nil {
		list = []string{}
	}
	return &RegionListResult{Loaded: true, List: list}
}

var geoDatLock sync.Mutex

// writeGeoDat 生成 dat 文件并返回内容哈希（8 位十六进制）。
//
// 内容没变且文件还在时不重写：本函数每 10 秒被走到一次。但只按哈希判断
// 是不够的——有人手工删掉文件后，xray 下次启动会因为找不到 dat 而失败，
// 那是全员断网，所以文件不在时必须重建。
func writeGeoDat(path string, entries []geodatEntry) (string, error) {
	geoDatLock.Lock()
	defer geoDatLock.Unlock()

	data, err := geodat.Encode(entries)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:4])

	if existing, err := os.ReadFile(path); err == nil {
		existingSum := sha256.Sum256(existing)
		if hex.EncodeToString(existingSum[:4]) == hash && len(existing) == len(data) {
			return hash, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return hash, nil
}
