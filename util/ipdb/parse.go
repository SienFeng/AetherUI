package ipdb

import (
	"encoding/binary"
	"os"
	"time"

	"a-ui/util/common"
)

// Parse 解析 Build 生成的字节。所有长度都逐段校验：一份被截断的库如果被当成
// 正常库加载，省份 CIDR 集合会静默变小，地区限制的取反规则就会误拒合法用户。
func Parse(data []byte) (*DB, error) {
	if len(data) < headerSize {
		return nil, common.NewErrorf("文件过短: %d 字节", len(data))
	}
	if string(data[:len(magic)]) != magic {
		return nil, common.NewError("文件头标识不匹配，不是 ipdb 数据文件")
	}
	if v := binary.LittleEndian.Uint32(data[8:]); v != formatVersion {
		return nil, common.NewErrorf("格式版本为 %d，本程序只支持 %d", v, formatVersion)
	}
	builtAt := time.Unix(int64(binary.LittleEndian.Uint64(data[12:])), 0)
	segCount := int(binary.LittleEndian.Uint32(data[20:]))
	locCount := int(binary.LittleEndian.Uint16(data[24:]))

	off := headerSize
	if need := off + segCount*segmentSize; len(data) < need {
		return nil, common.NewErrorf("段数据被截断: 需要 %d 字节，实际 %d", need, len(data))
	}
	segments := make([]segment, segCount)
	for i := 0; i < segCount; i++ {
		b := data[off+i*segmentSize:]
		segments[i] = segment{
			start: binary.LittleEndian.Uint32(b[0:]),
			end:   binary.LittleEndian.Uint32(b[4:]),
			loc:   binary.LittleEndian.Uint16(b[8:]),
		}
	}
	off += segCount * segmentSize

	if need := off + locCount*locationSize; len(data) < need {
		return nil, common.NewErrorf("归属地表被截断: 需要 %d 字节，实际 %d", need, len(data))
	}
	rawLocs := make([][3]uint16, locCount)
	for i := 0; i < locCount; i++ {
		b := data[off+i*locationSize:]
		rawLocs[i] = [3]uint16{
			binary.LittleEndian.Uint16(b[0:]),
			binary.LittleEndian.Uint16(b[2:]),
			binary.LittleEndian.Uint16(b[4:]),
		}
	}
	off += locCount * locationSize

	if len(data) < off+4 {
		return nil, common.NewError("字符串区被截断")
	}
	strCount := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	pool := make([]string, strCount)
	for i := 0; i < strCount; i++ {
		if len(data) < off+2 {
			return nil, common.NewErrorf("第 %d 个字符串的长度字段被截断", i)
		}
		n := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		if len(data) < off+n {
			return nil, common.NewErrorf("第 %d 个字符串被截断", i)
		}
		pool[i] = string(data[off : off+n])
		off += n
	}

	locations := make([]Location, locCount)
	for i, raw := range rawLocs {
		for _, idx := range raw {
			if int(idx) >= strCount {
				return nil, common.NewErrorf("归属地 %d 引用了越界的字符串下标 %d", i, idx)
			}
		}
		locations[i] = Location{
			Country: pool[raw[0]],
			Region:  pool[raw[1]],
			City:    pool[raw[2]],
		}
	}
	for i, s := range segments {
		if int(s.loc) >= locCount {
			return nil, common.NewErrorf("第 %d 段引用了越界的归属地下标 %d", i, s.loc)
		}
	}

	return &DB{builtAt: builtAt, segments: segments, locations: locations}, nil
}

// Load 从磁盘读入一份 ipdb 数据文件。
func Load(path string) (*DB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}
