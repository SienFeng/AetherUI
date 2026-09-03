// Package geodat 生成 v2ray/xray 的 GeoIP dat 文件。
//
// xray 的路由 source 条件可以写成 ext:<文件>:<TAG>，还支持 ext:<文件>:!<TAG>
// 取反。地区限制正是靠这个实现的：dat 里存「允许的省份对应的 IP 段」，
// 规则写成「来源不在这个集合里 → 黑洞」。
//
// 只做编码，不做解码：读回来是 xray 的事。protobuf 定义如下，字段号写死在
// 常量里，不引入 protobuf 运行时。
//
//	CIDR      { bytes ip = 1; uint32 prefix = 2; }
//	GeoIP     { string country_code = 1; repeated CIDR cidr = 2; }
//	GeoIPList { repeated GeoIP entry = 1; }
package geodat

import (
	"fmt"
	"net"
	"strings"
)

// protobuf 的 tag 字节 =（字段号 << 3）| wire type。
// wire type 2 是长度前缀，0 是 varint。
const (
	tagCIDRIP        = 0x0a // CIDR.ip，字段 1，长度前缀
	tagCIDRPrefix    = 0x10 // CIDR.prefix，字段 2，varint
	tagGeoIPCountry  = 0x0a // GeoIP.country_code，字段 1，长度前缀
	tagGeoIPCIDR     = 0x12 // GeoIP.cidr，字段 2，长度前缀
	tagGeoIPListItem = 0x0a // GeoIPList.entry，字段 1，长度前缀
)

// Entry 是 dat 里的一组 IP 段，Tag 就是路由条件里 ext:file:TAG 的那个 TAG。
type Entry struct {
	Tag   string
	CIDRs []string
}

// Encode 把若干组 IP 段编码成 dat 文件内容。
//
// 输出逐字节确定：entry 与 CIDR 都按传入顺序编码，调用方负责先排好序。
// 不确定的话，嵌进 ruleTag 的内容哈希会每轮都变，那个 10 秒的重启 cron
// 会不停重启 xray。
func Encode(entries []Entry) ([]byte, error) {
	seen := make(map[string]bool, len(entries))
	var out []byte

	for _, e := range entries {
		tag := strings.ToUpper(strings.TrimSpace(e.Tag))
		if tag == "" {
			return nil, fmt.Errorf("geodat: tag 不能为空")
		}
		// dat 里的 tag 查找大小写不敏感，重复 tag 的行为未经验证。
		// 生成端按省份集合去重，本就不该出现重复，这里挡死。
		if seen[tag] {
			return nil, fmt.Errorf("geodat: tag 重复: %s", tag)
		}
		seen[tag] = true

		if len(e.CIDRs) == 0 {
			// 空集合会让「来源不在集合里」匹配到所有人，整个入站被封死。
			return nil, fmt.Errorf("geodat: tag %s 的 IP 段列表为空", tag)
		}

		var geoip []byte
		geoip = appendBytesField(geoip, tagGeoIPCountry, []byte(tag))
		for _, c := range e.CIDRs {
			encoded, err := encodeCIDR(c)
			if err != nil {
				return nil, fmt.Errorf("geodat: tag %s: %w", tag, err)
			}
			geoip = appendBytesField(geoip, tagGeoIPCIDR, encoded)
		}
		out = appendBytesField(out, tagGeoIPListItem, geoip)
	}
	return out, nil
}

func encodeCIDR(s string) ([]byte, error) {
	_, network, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("网段 %q 非法: %w", s, err)
	}
	ip := network.IP
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else {
		ip = ip.To16()
	}
	ones, _ := network.Mask.Size()

	var out []byte
	out = appendBytesField(out, tagCIDRIP, ip)
	out = append(out, tagCIDRPrefix)
	out = appendVarint(out, uint64(ones))
	return out, nil
}

func appendBytesField(dst []byte, tag byte, payload []byte) []byte {
	dst = append(dst, tag)
	dst = appendVarint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}
