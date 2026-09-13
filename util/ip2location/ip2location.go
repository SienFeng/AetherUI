// Package ip2location 解析 IP2Location LITE DB3 的 CSV 发行包。
//
// 它是本项目的第三个离线归属地数据源。与 ip2region、纯真库的关系是「旁证」而
// 不是「更准」：实测四个源对同一批中国 IP 的省级判定两两一致度在 75%~93% 之间，
// 没有哪一个是权威，多一个源的价值在于让分歧显形。
//
// 上游是一个 ZIP，里面除 CSV 外还有 LICENSE_LITE.TXT 与 README_LITE.TXT。
// 行格式（全部带引号，逗号分隔）：
//
//	"ip_from","ip_to","country_code","country_name","region_name","city_name"
//
// 起止 IP 是**十进制整数**而不是点分十进制；字段缺失时上游写 "-" 而不是空串。
//
// 许可：LITE 数据可自由使用但要求署名，且**不得再分发**——所以数据只能由每台
// 面板用管理员自己的下载 token 获取，绝不能打进发版包。署名文案见
// LICENSE_LITE.TXT，界面上需要展示。
package ip2location

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"strconv"
	"strings"

	"a-ui/util/common"
	"a-ui/util/ipdb"
)

// maxSourceBytes 是 ZIP 的大小上限。DB3 的 IPv4 CSV 包实测约 33 MB，留足余量；
// 上限本身是为了挡住「URL 填错、对面返回一个几 GB 的东西」这类情况。
const maxSourceBytes = 200 << 20

// Parse 把 DB3 的 CSV 发行包解析成 ipdb 的原始数据段，按起始 IP 升序。
//
// 相邻且归属地相同的段在这里就合并，不留给 BuildRecords：上游有近 300 万行，
// 逐行留一个 Record 会在解析阶段就占掉数百 MB，而面板常跑在小内存 VPS 上。
func Parse(r io.Reader) ([]ipdb.Record, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSourceBytes {
		return nil, common.NewErrorf("IP2Location 源数据超过 %d 字节上限，疑似地址有误", maxSourceBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, common.NewErrorf("IP2Location: 不是有效的 ZIP: %v", err)
	}
	for _, f := range zr.File {
		if !strings.HasSuffix(strings.ToUpper(f.Name), ".CSV") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return parseCSV(rc)
	}
	return nil, common.NewError("IP2Location: ZIP 里没有 CSV 文件")
}

func parseCSV(r io.Reader) ([]ipdb.Record, error) {
	cr := csv.NewReader(r)
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
			return nil, common.NewErrorf("IP2Location: 第 %d 行解析失败: %v", lineNo+1, err)
		}
		lineNo++
		if len(fields) < 6 {
			return nil, common.NewErrorf("IP2Location: 第 %d 行只有 %d 个字段，应为 6", lineNo, len(fields))
		}
		start, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 32)
		if err != nil {
			return nil, common.NewErrorf("IP2Location: 第 %d 行起始 IP 无效: %v", lineNo, err)
		}
		end, err := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 32)
		if err != nil {
			return nil, common.NewErrorf("IP2Location: 第 %d 行结束 IP 无效: %v", lineNo, err)
		}
		if start > end {
			return nil, common.NewErrorf("IP2Location: 第 %d 行起始 IP 大于结束 IP", lineNo)
		}
		rec, ok := ipdb.LatinRecord(uint32(start), uint32(end),
			fields[2], dash(fields[4]), dash(fields[5]))
		if !ok {
			// 国家码不认识（含上游用 "-" 表示的保留段），整条丢弃。
			continue
		}
		records = appendMerged(records, rec)
	}
	if len(records) == 0 {
		return nil, common.NewError("IP2Location: 没有解析出任何数据段")
	}
	return records, nil
}

// dash 把上游表示「无此字段」的 "-" 换成空串。
//
// 不处理的话，中国段会出现一个叫「-」的省份：ProvinceFromLatin 认不出它、于是
// 省份留空，结果是对的；但城市那一路会拿 "-" 去查表，同样查不到、同样留空。
// 两条路径都碰巧无害，可这是巧合而不是设计，显式转换才靠得住。
func dash(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

// appendMerged 追加一段，能与上一段合并就合并。
//
// 合并条件与 BuildRecords 里的那段一致：紧邻且归属地完全相同。两处都做不是
// 重复——这里是为了压住解析阶段的内存峰值，那里是为了压住最终文件的段数。
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
