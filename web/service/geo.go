package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"a-ui/database/model"
	"a-ui/util/common"
	"a-ui/util/geodat"
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

// provinceCIDRSource 是 buildGeoPlan 需要的全部能力，方便测试替换。
type provinceCIDRSource interface {
	CIDRsOfProvinces(provinces []string) []string
}

// geoPlan 是一轮生成的结果：dat 里要写哪几组数据，以及每个入站用哪个 tag。
type geoPlan struct {
	Entries      []geodatEntry
	TagByInbound map[int]string
}

// buildGeoPlan 按省份集合去重，算出 dat 的内容与每个入站对应的 tag。
func buildGeoPlan(inbounds []*model.Inbound, db provinceCIDRSource) (*geoPlan, error) {
	plan := &geoPlan{TagByInbound: map[int]string{}}
	byTag := map[string][]string{}

	for _, in := range inbounds {
		if !in.Enable {
			continue
		}
		provinces, err := DecodeRegions(in.Regions)
		if err != nil {
			return nil, common.NewError("入站的地区数据损坏, id:", in.Id, "err:", err)
		}
		if len(provinces) == 0 {
			continue
		}
		tag := regionTag(provinces)
		plan.TagByInbound[in.Id] = tag
		if _, ok := byTag[tag]; ok {
			continue
		}
		cidrs := db.CIDRsOfProvinces(provinces)
		if len(cidrs) == 0 {
			// 允许集为空会让「来源不在集合里」匹配到所有人，该入站被彻底封死。
			// 报错让整份配置生成失败，好过生成一条把人全挡在门外的规则。
			return nil, common.NewError("找不到这些地区的 IP 段, 入站 id:", in.Id,
				"地区:", strings.Join(provinces, "、"), "（IP 归属地库可能未加载或地区名已变更）")
		}
		byTag[tag] = cidrs
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
	return plan, nil
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

func rulesFromGeoPlan(inbounds []*model.Inbound, plan *geoPlan, datHash string) []any {
	rules := make([]any, 0, len(plan.TagByInbound)*2)
	// 按入站 id 升序遍历，不遍历 map。
	for _, in := range inbounds {
		tag, ok := plan.TagByInbound[in.Id]
		if !ok {
			continue
		}
		rules = append(rules, map[string]any{
			"type":        "field",
			"ruleTag":     "a-ui-geo6-" + itoa(in.Id),
			"inboundTag":  []string{in.Tag},
			"source":      []string{"::/0"},
			"outboundTag": model.BlockOutboundTag,
		})
		rules = append(rules, map[string]any{
			"type": "field",
			// dat 的内容变化不体现在配置字节里，Config.Equals 察觉不到，
			// xray 也就不会重新加载。把内容哈希嵌进 ruleTag，哈希变则配置
			// 字节变则重启，不必为 dat 单独造一套变更检测。
			"ruleTag":     "a-ui-geo-" + itoa(in.Id) + "-" + datHash,
			"inboundTag":  []string{in.Tag},
			"source":      []string{"ext:" + geoDatRef + ":!" + tag},
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
