// Package dbip 解析 DB-IP City Lite 的 CSV 发行包。
//
// 它是本项目的第四个离线归属地数据源，定位与 util/ip2location 相同：旁证，不是
// 更准。实测它与其余三源的平均省级一致度是四个里最低的（80.2%），并且把 27.2%
// 的中国段兜底到广东省（ip2region 是 9.7%），所以它在 ipdbSourceList 里排末位，
// 只在前面几个源都没有数据时才成为主判定。
//
// 上游是 gzip 压缩的 CSV，行格式（部分字段带引号）：
//
//	ip_from,ip_to,continent_code,country_code,region_name,city_name,latitude,longitude
//
// 三处与 IP2Location 不同，都必须处理：
//
//   - 起止 IP 是点分十进制，**且 IPv4 与 IPv6 混在同一个文件里**；本项目只收 IPv4
//     （ipdb.Record 的 Start/End 是 uint32），IPv6 行必须跳过。
//   - 没有 country_name，只有国家码，国家名只能靠 ipdb.CountryFromCode 查。
//   - 经纬度用不上：ipdb.Location 没有这两个字段，本项目也不需要。
//
// 许可：CC BY 4.0，要求署名，界面上需要展示。
package dbip

import (
	"compress/gzip"
	"encoding/csv"
	"io"
	"strings"

	"a-ui/util/common"
	"a-ui/util/ipdb"
)

const (
	// maxSourceBytes 是压缩流的大小上限。City Lite 的 csv.gz 实测约 82 MB。
	maxSourceBytes = 300 << 20
	// maxRecords 挡住解压后异常膨胀的输入。上游约 774 万行，合并后远少于此。
	maxRecords = 20 << 20
)

// Parse 把 City Lite 的 csv.gz 解析成 ipdb 的原始数据段，按起始 IP 升序。
//
// 全程流式：上游解压后有几百 MB，读进内存会在小内存 VPS 上直接 OOM。
// 相邻且归属地相同的段在这里就合并，理由同 util/ip2location。
func Parse(r io.Reader) ([]ipdb.Record, error) {
	zr, err := gzip.NewReader(io.LimitReader(r, maxSourceBytes))
	if err != nil {
		return nil, common.NewErrorf("DB-IP: 不是有效的 gzip: %v", err)
	}
	defer zr.Close()

	cr := csv.NewReader(zr)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true

	var (
		records []ipdb.Record
		lineNo  int
	)
	for {
		fields, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, common.NewErrorf("DB-IP: 第 %d 行解析失败: %v", lineNo+1, err)
		}
		lineNo++
		if len(fields) < 6 {
			return nil, common.NewErrorf("DB-IP: 第 %d 行只有 %d 个字段，应为 6 个以上", lineNo, len(fields))
		}
		// IPv6 行直接跳过，不报错：它们是上游的正常内容，不是数据损坏。
		// 跳过之后 IPv4 段之间的相对顺序不变，仍满足 BuildRecords 的升序要求。
		if strings.ContainsRune(fields[0], ':') {
			continue
		}
		start, ok := parseIPv4(fields[0])
		if !ok {
			return nil, common.NewErrorf("DB-IP: 第 %d 行起始 IP 无效: %q", lineNo, fields[0])
		}
		end, ok := parseIPv4(fields[1])
		if !ok {
			return nil, common.NewErrorf("DB-IP: 第 %d 行结束 IP 无效: %q", lineNo, fields[1])
		}
		if start > end {
			return nil, common.NewErrorf("DB-IP: 第 %d 行起始 IP 大于结束 IP", lineNo)
		}
		rec, ok2 := ipdb.LatinRecord(start, end, fields[3], fields[4], fields[5])
		if !ok2 {
			// 国家码不认识（含上游用 "ZZ" 表示的保留段），整条丢弃。
			continue
		}
		records = appendMerged(records, rec)
		if len(records) > maxRecords {
			return nil, common.NewErrorf("DB-IP: 数据段超过 %d 条上限，疑似地址有误", maxRecords)
		}
	}
	if len(records) == 0 {
		return nil, common.NewError("DB-IP: 没有解析出任何 IPv4 数据段")
	}
	return records, nil
}

// parseIPv4 把点分十进制解析成 uint32。
//
// 不用 net.ParseIP：上游有近八百万行，那条路径每行都要分配一个 16 字节切片。
func parseIPv4(s string) (uint32, bool) {
	var (
		v    uint32
		part uint32
		dots int
		seen bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			part = part*10 + uint32(c-'0')
			if part > 255 {
				return 0, false
			}
			seen = true
		case c == '.':
			if !seen || dots == 3 {
				return 0, false
			}
			v = v<<8 | part
			part, seen = 0, false
			dots++
		default:
			return 0, false
		}
	}
	if !seen || dots != 3 {
		return 0, false
	}
	return v<<8 | part, true
}

// appendMerged 追加一段，能与上一段合并就合并。合并条件与 BuildRecords 一致。
func appendMerged(records []ipdb.Record, rec ipdb.Record) []ipdb.Record {
	if n := len(records); n > 0 {
		p := &records[n-1]
		if p.End+1 == rec.Start && p.Country == rec.Country &&
			p.Region == rec.Region && p.City == rec.City && p.ISP == rec.ISP {
			p.End = rec.End
			return records
		}
	}
	return append(records, rec)
}
