// Package qqwry 解析纯真 IP 数据库（qqwry.dat）。
//
// 它是本项目的第二个离线归属地数据源，与 ip2region 并存：两者对同一个 IP
// 的判定时有出入，多一个源可以交叉校验，也能在某个源的下载地址失效时兜底。
//
// 线格式（全部小端）：
//
//	头部 8 字节：索引区起始偏移、索引区结束偏移
//	索引项 7 字节：起始 IP（uint32）+ 记录偏移（uint24）
//	记录：结束 IP（uint32）+ 国家 + 地区
//
// 国家与地区都可能被 0x01（整条重定向）或 0x02（仅字符串重定向）指到别处，
// 字符串是 GBK 编码、以 \0 结尾。这些重定向必须递归跟进——真实库里这类
// 记录占了相当比例，不处理会解出乱码。
package qqwry

import (
	"encoding/binary"
	"strings"

	"a-ui/util/common"
	"a-ui/util/ipdb"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	headerLen = 8
	indexLen  = 7

	// maxRedirect 是重定向链的深度上限。数据损坏时重定向可能成环，
	// 不设上限会直接栈溢出——而这段代码跑在定时更新任务里，
	// panic 会杀掉整个面板进程。
	maxRedirect = 8

	// areaSeparator 是纯真库里「中国–江苏–南京」的分隔符，
	// 是 U+2013 EN DASH，不是普通连字符。
	areaSeparator = "–"

	chinaCountry = "中国"
)

// Parse 把 qqwry.dat 的内容解析成 ipdb 的原始数据段，按起始 IP 升序。
func Parse(data []byte) ([]ipdb.Record, error) {
	if len(data) < headerLen {
		return nil, common.NewError("qqwry: 文件过短")
	}
	indexStart := binary.LittleEndian.Uint32(data[0:])
	indexEnd := binary.LittleEndian.Uint32(data[4:])
	if indexStart < headerLen || indexEnd < indexStart {
		return nil, common.NewErrorf("qqwry: 索引区范围非法: %d~%d", indexStart, indexEnd)
	}
	if uint64(indexEnd)+indexLen > uint64(len(data)) {
		return nil, common.NewErrorf("qqwry: 索引区越界: %d~%d，文件 %d 字节",
			indexStart, indexEnd, len(data))
	}
	count := (indexEnd-indexStart)/indexLen + 1

	records := make([]ipdb.Record, 0, count)
	for i := uint32(0); i < count; i++ {
		off := indexStart + i*indexLen
		start := binary.LittleEndian.Uint32(data[off:])
		recOff := readU24(data[off+4:])
		if uint64(recOff)+4 > uint64(len(data)) {
			return nil, common.NewErrorf("qqwry: 第 %d 条索引的记录偏移越界: %d", i+1, recOff)
		}
		end := binary.LittleEndian.Uint32(data[recOff:])

		country, area, err := resolve(data, recOff+4, 0)
		if err != nil {
			return nil, common.NewErrorf("qqwry: 第 %d 条记录解析失败: %v", i+1, err)
		}
		rec := toRecord(start, end, country, area)
		records = append(records, rec)
	}
	return records, nil
}

// resolve 读出一条记录的国家与地区，跟进 0x01 / 0x02 重定向。
//
// pos 指向「国家位置」，也就是记录偏移 + 4（跳过 4 字节的结束 IP）。
func resolve(data []byte, pos uint32, depth int) (country, area string, err error) {
	if depth > maxRedirect {
		return "", "", common.NewError("重定向层数过深，数据可能已损坏或成环")
	}
	if uint64(pos) >= uint64(len(data)) {
		return "", "", common.NewErrorf("偏移越界: %d", pos)
	}

	switch data[pos] {
	case 0x01:
		// 整条重定向：国家与地区都在别处。
		if uint64(pos)+4 > uint64(len(data)) {
			return "", "", common.NewError("0x01 重定向的偏移被截断")
		}
		return resolve(data, readU24(data[pos+1:]), depth+1)
	case 0x02:
		// 只有国家被重定向，地区紧跟在这 4 个字节之后。
		if uint64(pos)+4 > uint64(len(data)) {
			return "", "", common.NewError("0x02 重定向的偏移被截断")
		}
		country, err = readString(data, readU24(data[pos+1:]), depth+1)
		if err != nil {
			return "", "", err
		}
		area, err = readString(data, pos+4, depth+1)
		if err != nil {
			return "", "", err
		}
		return country, area, nil
	default:
		country, err = readString(data, pos, depth+1)
		if err != nil {
			return "", "", err
		}
		next := pos + uint32(gbkLen(data, pos)) + 1
		area, err = readString(data, next, depth+1)
		if err != nil {
			return "", "", err
		}
		return country, area, nil
	}
}

// readString 读一个 GBK 字符串，自身也可能被重定向。
func readString(data []byte, pos uint32, depth int) (string, error) {
	if depth > maxRedirect {
		return "", common.NewError("字符串重定向层数过深")
	}
	if uint64(pos) >= uint64(len(data)) {
		return "", common.NewErrorf("字符串偏移越界: %d", pos)
	}
	if data[pos] == 0x01 || data[pos] == 0x02 {
		if uint64(pos)+4 > uint64(len(data)) {
			return "", common.NewError("字符串重定向的偏移被截断")
		}
		return readString(data, readU24(data[pos+1:]), depth+1)
	}
	n := gbkLen(data, pos)
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(data[pos : pos+uint32(n)])
	if err != nil {
		// 解不出来就当空串：单条记录的编码问题不该让整份库作废。
		return "", nil
	}
	return string(decoded), nil
}

// gbkLen 返回从 pos 开始到 \0 之前的字节数。没有 \0 时读到文件末尾。
func gbkLen(data []byte, pos uint32) int {
	for i := pos; uint64(i) < uint64(len(data)); i++ {
		if data[i] == 0 {
			return int(i - pos)
		}
	}
	return len(data) - int(pos)
}

func readU24(b []byte) uint32 {
	if len(b) < 3 {
		return 0
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}

// toRecord 把纯真库的自由文本归属地折成结构化的国家 / 省 / 市。
//
// 格式是「中国–江苏–南京」，但并非每条都这样：云厂商 anycast 段可能只有
// 「腾讯云」或「中国」。**认不出省份时留空，绝不硬塞**——把「腾讯云」当成
// 省份会污染整个允许集，生成的 CIDR 会把一批本不该放行的段带进来。
func toRecord(start, end uint32, country, area string) ipdb.Record {
	_ = area // ISP 信息本包不保留，与 ip2region 那一路口径一致
	parts := strings.Split(strings.TrimSpace(country), areaSeparator)
	rec := ipdb.Record{Start: start, End: end, Country: strings.TrimSpace(parts[0])}
	if rec.Country != chinaCountry {
		// 境外只保留国家，与 ip2region 那一路一致。
		return rec
	}
	if len(parts) < 2 {
		return rec
	}
	province, ok := ipdb.CanonicalProvince(parts[1])
	if !ok {
		return rec
	}
	rec.Region = province
	if len(parts) >= 3 {
		rec.City = strings.TrimSpace(parts[2])
	}
	return rec
}
