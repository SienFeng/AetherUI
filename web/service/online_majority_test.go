package service

import (
	"bytes"
	"net"
	"testing"
	"time"

	"a-ui/util/ipdb"
)

// ipdbServiceWithDBs 按给定顺序装入任意多个数据源。
//
// 既有的 ipdbServiceWithSources 固定两个源，测不了多数票——省份要分出胜负
// 至少得有三个源，而真实的 ipdbSourceList 是四个。
func ipdbServiceWithDBs(t *testing.T, dbs ...namedDB) IPDBService {
	t.Helper()
	dir := t.TempDir()
	sources := make([]ipdbSource, len(dbs))
	for i, d := range dbs {
		sources[i] = testSource(dir+"/"+d.name+".dat", 1)
		sources[i].Key, sources[i].Name = d.name, d.name
	}
	useTestSources(t, sources)

	s := IPDBService{}
	for _, d := range dbs {
		s.setDB(d.name, d.db)
	}
	return s
}

// 生产实测形状（223.160.117.162，取自香港机器上的四个真实库）：ip2region 排在
// 第一位却是少数派，两个源都说湖北。改动前界面显示「中国 北京市 北京市」，
// tooltip 自己算出的证据行却写着「湖北省 2/4 源支持」——面板掌握着正确答案
// 却没有用它。
func TestLocateWithIPDBPicksMajorityProvince(t *testing.T) {
	svc := ipdbServiceWithDBs(t,
		namedDB{"ip2region", buildLocTestDB(t, "北京市", "北京市", "中国移动")},
		namedDB{"纯真", buildLocTestDB(t, "湖北省", "", "中国广电")},
		namedDB{"IP2Location", buildLocTestDB(t, "", "", "")},
		namedDB{"DB-IP", buildLocTestDB(t, "湖北省", "", "")})

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))

	if got.Location != "中国 湖北省" {
		t.Errorf("Location = %q，想要 %q", got.Location, "中国 湖北省")
	}
	// 整组取自同一个源：省份是纯真给的，运营商也该是纯真的中国广电，而不是
	// ip2region 的中国移动。
	if got.ISP != "中国广电" {
		t.Errorf("ISP = %q，想要 %q", got.ISP, "中国广电")
	}
	// 少数派仍要作为「存疑」的指向显示出来，不能因为落选就消失。
	if got.LocationAlt != "中国 北京市 北京市" {
		t.Errorf("LocationAlt = %q，想要 %q", got.LocationAlt, "中国 北京市 北京市")
	}
}

// buildLocTestDBCountry 与 buildLocTestDB 同构，但国家可指定——境外段的形状
// （只有国家、没有省份）是回落路径的入口之一，用固定「中国」的那个造不出来。
func buildLocTestDBCountry(t *testing.T, country, region, city, isp string) *ipdb.DB {
	t.Helper()
	var buf bytes.Buffer
	recs := []ipdb.Record{{
		Start: 0x01000000, End: 0x0100FFFF,
		Country: country, Region: region, City: city, ISP: isp,
	}}
	if err := ipdb.BuildRecords(recs, &buf, time.Unix(1788000000, 0)); err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	db, err := ipdb.Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return db
}

// 平票不进入仲裁，一律维持改动前的顺序优先。
//
// 用例刻意让「字节序最小的省」与「第一个源的省」不是同一个：evidenceOf 平票时
// 把 Province 定成北京市（北 E58C97 < 湖 E6B996），而正确行为是回落到排第一的
// ip2region 给出的湖北省。照着 Evidence.Province 走就会显示北京市。
func TestLocateWithIPDBFallsBackToSourceOrderOnTie(t *testing.T) {
	svc := ipdbServiceWithDBs(t,
		namedDB{"ip2region", buildLocTestDB(t, "湖北省", "", "")},
		namedDB{"纯真", buildLocTestDB(t, "北京市", "", "")},
		namedDB{"IP2Location", buildLocTestDB(t, "湖北省", "", "")},
		namedDB{"DB-IP", buildLocTestDB(t, "北京市", "", "")})

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))

	if got.Location != "中国 湖北省" {
		t.Errorf("Location = %q，想要 %q（2:2 平票应当维持顺序优先）", got.Location, "中国 湖北省")
	}
}

// 一个源都没给出省份时（境外段的常态）同样回落顺序优先。
func TestLocateWithIPDBFallsBackWhenNoSourceHasProvince(t *testing.T) {
	svc := ipdbServiceWithDBs(t,
		namedDB{"ip2region", buildLocTestDBCountry(t, "Australia", "", "", "")},
		namedDB{"纯真", buildLocTestDBCountry(t, "Japan", "", "", "")})

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))

	if got.Location != "Australia" {
		t.Errorf("Location = %q，想要 %q", got.Location, "Australia")
	}
}

// 省份定下之后，城市在同省的源之间再投一次票。
func TestLocateWithIPDBPicksMajorityCityWithinProvince(t *testing.T) {
	svc := ipdbServiceWithDBs(t,
		namedDB{"ip2region", buildLocTestDB(t, "江苏省", "南京市", "中国电信")},
		namedDB{"纯真", buildLocTestDB(t, "江苏省", "无锡市", "中国电信")},
		namedDB{"DB-IP", buildLocTestDB(t, "江苏省", "无锡市", "")})

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))

	if got.Location != "中国 江苏省 无锡市" {
		t.Errorf("Location = %q，想要 %q", got.Location, "中国 江苏省 无锡市")
	}
}

// 城市票必须先按 CanonicalCity 归一再数：ip2region 写「无锡市」、纯真写「无锡」，
// 不归一的话同一个城市被拆成两个候选，三方各 1 票落进平票分支，显示的就成了
// 排第一的那个源给出的南京市。
func TestLocateWithIPDBCanonicalizesCityBeforeVoting(t *testing.T) {
	svc := ipdbServiceWithDBs(t,
		namedDB{"ip2region", buildLocTestDB(t, "江苏省", "南京市", "")},
		namedDB{"纯真", buildLocTestDB(t, "江苏省", "无锡市", "")},
		namedDB{"DB-IP", buildLocTestDB(t, "江苏省", "无锡", "")})

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))

	// 显示用支持该归一名的第一个源的原文，不自己拼写法。
	if got.Location != "中国 江苏省 无锡市" {
		t.Errorf("Location = %q，想要 %q", got.Location, "中国 江苏省 无锡市")
	}
}

// 城市平票时按子集内的源顺序取，与省份平票的处置同构。
func TestLocateWithIPDBFallsBackOnCityTie(t *testing.T) {
	svc := ipdbServiceWithDBs(t,
		namedDB{"ip2region", buildLocTestDB(t, "江苏省", "南京市", "")},
		namedDB{"纯真", buildLocTestDB(t, "江苏省", "无锡市", "")})

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))

	if got.Location != "中国 江苏省 南京市" {
		t.Errorf("Location = %q，想要 %q", got.Location, "中国 江苏省 南京市")
	}
}

// 胜出那一批源全都不提供运营商时，运营商回落全集——两个拉丁源从不带 ISP，
// 锁死在子集里会让这一列凭空消失。
func TestLocateWithIPDBFallsBackForISPWhenWinnersHaveNone(t *testing.T) {
	svc := ipdbServiceWithDBs(t,
		namedDB{"ip2region", buildLocTestDB(t, "北京市", "北京市", "中国移动")},
		namedDB{"IP2Location", buildLocTestDB(t, "湖北省", "", "")},
		namedDB{"DB-IP", buildLocTestDB(t, "湖北省", "", "")})

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))

	if got.Location != "中国 湖北省" {
		t.Errorf("Location = %q，想要 %q", got.Location, "中国 湖北省")
	}
	if got.ISP != "中国移动" {
		t.Errorf("ISP = %q，想要 %q（子集内无人提供时回落全集）", got.ISP, "中国移动")
	}
	// 全集里只有这一个运营商，没有第二种说法可报。
	if got.ISPAlt != "" {
		t.Errorf("ISPAlt = %q，想要空", got.ISPAlt)
	}
}
