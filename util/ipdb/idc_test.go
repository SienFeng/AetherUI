package ipdb

import (
	"reflect"
	"testing"
)

// 机房 / 云厂商的网段按多数票判：支持 = 认为这段是知名 IDC 的源，反对 = 认为它是别的运营商（家宽、手机）的源，
// 没有运营商信息的源不表态。一个源把家宽误标成机房，不能因此把那片用户全拦掉。
//
// 已知局限（见 MajorityCIDRsOfIDC 的注释）：境外段的非机房运营商在建库时被清空、投不出反对票，所以境外段
// 只要一个源说是机房就拦（3.0.0.0/24）。夹具按真实建库的归一结果写（CanonicalISP），这条测试把局限钉住，
// 将来改判据时会在这里响亮地变红。
func TestMajorityCIDRsOfIDC(t *testing.T) {
	a := buildDB(t, []Record{
		{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "浙江省", ISP: "阿里云"},
		{Start: ipv4(2, 0, 0, 0), End: ipv4(2, 0, 0, 255), Country: "中国", Region: "江苏省", ISP: "阿里云"},
		{Start: ipv4(3, 0, 0, 0), End: ipv4(3, 0, 0, 255), Country: "美国", ISP: "Amazon"},
		{Start: ipv4(4, 0, 0, 0), End: ipv4(4, 0, 0, 255), Country: "美国", ISP: "Amazon"},
	})
	b := buildDB(t, []Record{
		{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "浙江省", ISP: "阿里云"},
		{Start: ipv4(2, 0, 0, 0), End: ipv4(2, 0, 0, 255), Country: "中国", Region: "江苏省", ISP: "电信"},
		{Start: ipv4(3, 0, 0, 0), End: ipv4(3, 0, 0, 255), Country: "美国", ISP: CanonicalISP("Comcast", true)},
		{Start: ipv4(4, 0, 0, 0), End: ipv4(4, 0, 0, 255), Country: "美国", ISP: CanonicalISP("Amazon AWS", true)},
	})
	c := buildDB(t, []Record{
		{Start: ipv4(2, 0, 0, 0), End: ipv4(2, 0, 0, 255), Country: "中国", Region: "江苏省", ISP: "电信"},
	})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}, {Key: "c", DB: c}})
	got := m.MajorityCIDRsOfIDC()
	want := []string{"1.0.0.0/24", "3.0.0.0/24", "4.0.0.0/24"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MajorityCIDRsOfIDC = %v，期望 %v（2.0.0.0/24 一个源说是机房、两个源说是电信；"+
			"3.0.0.0/24 境外段一个源说是机房、另一个源的 Comcast 建库时被清空——已知局限，仍会被拦）", got, want)
	}
}
