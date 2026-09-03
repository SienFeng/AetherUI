// Package ipdb 是一份离线的 IP 归属地库：既能按 IP 查国家/省/市（在线明细展示用），
// 也能按省份导出 CIDR 集合（生成 xray 的 geo 数据文件用）。
//
// 数据来自 ip2region 的 ipv4_source.txt（Apache-2.0 OR MIT，与本项目 GPL-3.0 兼容），
// 由 Build 转换成本包自定义的紧凑格式：35 MB 的文本压到约 2 MB，且可直接二分查找。
//
// 精度取舍：境外只保留国家，中国保留到省+市。上游把同一个城市按 ISP 拆成多段，
// 而本包不存 ISP，因此相邻同归属地的段会被合并掉。
package ipdb

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"time"

	"a-ui/util/common"
)

const (
	magic         = "AUIPDB01"
	formatVersion = uint32(1)

	headerSize   = 32
	segmentSize  = 10
	locationSize = 6

	chinaCountry = "中国"
)

// Record 是构建时的一条原始数据段。各数据源的解析器都产出这个类型，
// 由 BuildRecords 统一校验并写成紧凑格式。
type Record struct {
	Start, End uint32
	Country    string
	Region     string
	City       string
}

// Location 是一个 IP 段的归属地。境外段只有 Country，Region 与 City 为空。
type Location struct {
	Country string
	Region  string
	City    string
}

type segment struct {
	start, end uint32
	loc        uint16
}

type DB struct {
	builtAt   time.Time
	segments  []segment
	locations []Location
}

func (d *DB) SegmentCount() int { return len(d.segments) }

func (d *DB) BuiltAt() time.Time { return d.builtAt }

// Lookup 查询 IP 的归属地。只收录 IPv4，IPv6 一律返回 false。
func (d *DB) Lookup(ip net.IP) (Location, bool) {
	v4 := ip.To4()
	if v4 == nil {
		return Location{}, false
	}
	key := binary.BigEndian.Uint32(v4)

	lo, hi := 0, len(d.segments)-1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		s := d.segments[mid]
		switch {
		case key < s.start:
			hi = mid - 1
		case key > s.end:
			lo = mid + 1
		default:
			return d.locations[s.loc], true
		}
	}
	return Location{}, false
}

// normalize 把上游用来表示「无此字段」的占位值统一成空串，并按既定精度
// 丢掉境外段的省市。
func normalize(country, region, city string) Location {
	if country == "Reserved" || country == "0" {
		country = ""
	}
	if country != chinaCountry {
		return Location{Country: country}
	}
	if region == "0" {
		region = ""
	}
	if city == "0" {
		city = ""
	}
	return Location{Country: country, Region: region, City: city}
}

// Build 把上游的 ipv4_source.txt 转换成本包的紧凑格式。
//
// builtAt 由调用方传入而不是取当前时间，是为了让生成逐字节确定：同样的输入
// 必须产出完全相同的文件，否则每次更新都会在 git 里留下无意义的 diff，也无法校验。
//
// 遇到格式异常、乱序或重叠的输入一律报错而不是跳过：宁可更新失败保留旧库，
// 也不能生成一份内容残缺的库——归属地查空只是显示不准，而省份 CIDR 集合查空
// 会让地区限制的取反规则变成「拒绝所有人」。
func Build(src io.Reader, dst io.Writer, builtAt time.Time) error {
	records, err := parseIP2Region(src)
	if err != nil {
		return err
	}
	return BuildRecords(records, dst, builtAt)
}

// parseIP2Region 解析 ip2region 的 ipv4_source.txt。
func parseIP2Region(src io.Reader) ([]Record, error) {
	var (
		records []Record
		lineNo  int
	)

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lineNo++
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 7 {
			return nil, common.NewErrorf("第 %d 行字段数为 %d，应为 7: %q", lineNo, len(fields), line)
		}
		start, err := parseIPv4(fields[0])
		if err != nil {
			return nil, common.NewErrorf("第 %d 行起始 IP 无效: %v", lineNo, err)
		}
		end, err := parseIPv4(fields[1])
		if err != nil {
			return nil, common.NewErrorf("第 %d 行结束 IP 无效: %v", lineNo, err)
		}
		if start > end {
			return nil, common.NewErrorf("第 %d 行起始 IP 大于结束 IP: %q", lineNo, line)
		}
		loc := normalize(fields[2], fields[3], fields[4])
		records = append(records, Record{
			Start: start, End: end,
			Country: loc.Country, Region: loc.Region, City: loc.City,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// BuildRecords 把已解析的数据段写成本包的紧凑格式。
//
// 校验与 Build 相同：乱序、重叠一律报错而不是跳过，因为二分查找依赖
// 「整表升序且互不重叠」这个前提，而省份 CIDR 集合查空会让地区限制的
// 取反规则变成「拒绝所有人」。
func BuildRecords(records []Record, dst io.Writer, builtAt time.Time) error {
	var (
		segments  []segment
		locIndex  = map[Location]uint16{}
		locations []Location
		strIndex  = map[string]uint32{"": 0}
		strings_  = []string{""}
		hasPrev   bool
		prevEnd   uint32
	)

	intern := func(s string) uint32 {
		if i, ok := strIndex[s]; ok {
			return i
		}
		i := uint32(len(strings_))
		strIndex[s] = i
		strings_ = append(strings_, s)
		return i
	}

	for i, r := range records {
		if r.Start > r.End {
			return common.NewErrorf("第 %d 段起始 IP 大于结束 IP", i+1)
		}
		if hasPrev && r.Start <= prevEnd {
			return common.NewErrorf("第 %d 段与上一段重叠或乱序", i+1)
		}
		hasPrev, prevEnd = true, r.End

		loc := Location{Country: r.Country, Region: r.Region, City: r.City}
		id, ok := locIndex[loc]
		if !ok {
			if len(locations) >= 1<<16 {
				return common.NewError("归属地种类超过 65536，格式无法表达")
			}
			id = uint16(len(locations))
			locIndex[loc] = id
			locations = append(locations, loc)
			intern(loc.Country)
			intern(loc.Region)
			intern(loc.City)
		}

		if n := len(segments); n > 0 && segments[n-1].loc == id && segments[n-1].end+1 == r.Start {
			segments[n-1].end = r.End
			continue
		}
		segments = append(segments, segment{start: r.Start, end: r.End, loc: id})
	}
	if len(segments) == 0 {
		return common.NewError("输入没有任何有效数据段")
	}

	w := bufio.NewWriter(dst)
	header := make([]byte, headerSize)
	copy(header, magic)
	binary.LittleEndian.PutUint32(header[8:], formatVersion)
	binary.LittleEndian.PutUint64(header[12:], uint64(builtAt.Unix()))
	binary.LittleEndian.PutUint32(header[20:], uint32(len(segments)))
	binary.LittleEndian.PutUint16(header[24:], uint16(len(locations)))
	if _, err := w.Write(header); err != nil {
		return err
	}

	buf := make([]byte, segmentSize)
	for _, s := range segments {
		binary.LittleEndian.PutUint32(buf[0:], s.start)
		binary.LittleEndian.PutUint32(buf[4:], s.end)
		binary.LittleEndian.PutUint16(buf[8:], s.loc)
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}

	buf = buf[:locationSize]
	for _, loc := range locations {
		binary.LittleEndian.PutUint16(buf[0:], uint16(strIndex[loc.Country]))
		binary.LittleEndian.PutUint16(buf[2:], uint16(strIndex[loc.Region]))
		binary.LittleEndian.PutUint16(buf[4:], uint16(strIndex[loc.City]))
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}

	var n4 [4]byte
	binary.LittleEndian.PutUint32(n4[:], uint32(len(strings_)))
	if _, err := w.Write(n4[:]); err != nil {
		return err
	}
	var n2 [2]byte
	for _, s := range strings_ {
		if len(s) > 1<<16-1 {
			return common.NewError("字符串过长:", s)
		}
		binary.LittleEndian.PutUint16(n2[:], uint16(len(s)))
		if _, err := w.Write(n2[:]); err != nil {
			return err
		}
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
	}
	return w.Flush()
}

func parseIPv4(s string) (uint32, error) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return 0, common.NewError("不是合法 IP:", s)
	}
	v4 := ip.To4()
	if v4 == nil {
		return 0, common.NewError("不是 IPv4:", s)
	}
	return binary.BigEndian.Uint32(v4), nil
}
