package ipdb

import (
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 针对仓库里那份真实的 bin/ipdb.dat 的抽样断言。
//
// 用 runtime.Caller 定位仓库根而不是写相对路径：go test 的工作目录是包目录，
// 而这份数据文件在仓库根的 bin/ 下。
func realDB(t *testing.T) *DB {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "bin", "ipdb.dat")
	db, err := Load(path)
	if err != nil {
		// 这个文件是仓库跟踪的，缺失本身就是问题，不能跳过。
		t.Fatalf("加载 %s: %v", path, err)
	}
	return db
}

func TestRealDataLookupsKnownIPs(t *testing.T) {
	db := realDB(t)
	for _, c := range []struct{ ip, country, region, city string }{
		{"114.114.114.114", "中国", "江苏省", "南京市"},
		{"223.5.5.5", "中国", "浙江省", "杭州市"},
		{"202.96.209.5", "中国", "上海市", "上海市"},
		{"8.8.8.8", "United States", "", ""},
	} {
		loc, ok := db.Lookup(net.ParseIP(c.ip))
		if !ok {
			t.Errorf("Lookup(%s) 未命中", c.ip)
			continue
		}
		if loc.Country != c.country || loc.Region != c.region || loc.City != c.city {
			t.Errorf("Lookup(%s) = %q/%q/%q, want %q/%q/%q",
				c.ip, loc.Country, loc.Region, loc.City, c.country, c.region, c.city)
		}
	}
}

func TestRealDataProvincesCoverAllChineseRegions(t *testing.T) {
	db := realDB(t)
	got := db.Provinces()
	if len(got) != 34 {
		t.Errorf("省份数 = %d, want 34（31 个省级行政区 + 港澳台）", len(got))
	}
	index := map[string]bool{}
	for _, p := range got {
		index[p] = true
		if p == "0" || p == "" {
			t.Errorf("省份列表里混入了占位值 %q", p)
		}
	}
	for _, must := range []string{"江苏省", "河南省", "北京市", "西藏", "新疆", "香港特别行政区"} {
		if !index[must] {
			t.Errorf("省份列表缺少 %q，实际为 %v", must, got)
		}
	}
}

func TestRealDataProvinceCIDRsAreSaneAndSorted(t *testing.T) {
	db := realDB(t)
	cidrs := db.CIDRsOfProvinces([]string{"江苏省"})
	if len(cidrs) < 500 || len(cidrs) > 5000 {
		t.Errorf("江苏省 CIDR 条数 = %d，明显偏离预期量级（约 1300）", len(cidrs))
	}

	var prev uint32
	for i, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("第 %d 条 %q 不是合法 CIDR: %v", i, c, err)
		}
		v4 := n.IP.To4()
		if v4 == nil {
			t.Fatalf("第 %d 条 %q 不是 IPv4", i, c)
		}
		cur := uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
		if i > 0 && cur <= prev {
			t.Fatalf("第 %d 条 %q 未按 IP 升序（上一条起点 %d）", i, c, prev)
		}
		prev = cur
	}

	// 江苏的地址必须落在集合内，外省的必须不在——否则地区限制会放错人。
	inJiangsu := net.ParseIP("114.114.114.114")
	inZhejiang := net.ParseIP("223.5.5.5")
	var covered, wrong bool
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		if n.Contains(inJiangsu) {
			covered = true
		}
		if n.Contains(inZhejiang) {
			wrong = true
		}
	}
	if !covered {
		t.Error("江苏省的 CIDR 集合没有覆盖 114.114.114.114")
	}
	if wrong {
		t.Error("江苏省的 CIDR 集合错误地包含了浙江的 223.5.5.5")
	}
}

func TestRealDataSegmentsAreOrderedAndNonOverlapping(t *testing.T) {
	db := realDB(t)
	if db.SegmentCount() < 100000 {
		t.Fatalf("段数 = %d，数据文件疑似不完整", db.SegmentCount())
	}
	for i := 1; i < len(db.segments); i++ {
		if db.segments[i].start <= db.segments[i-1].end {
			t.Fatalf("第 %d 段与上一段重叠或乱序，二分查找前提被破坏", i)
		}
	}
}

func TestRealDataHasNoPlaceholderCountryNames(t *testing.T) {
	db := realDB(t)
	for _, loc := range db.locations {
		if strings.EqualFold(loc.Country, "Reserved") || loc.Country == "0" {
			t.Errorf("归属地里残留了占位值: %+v", loc)
		}
	}
}
