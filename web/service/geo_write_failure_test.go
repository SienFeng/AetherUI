package service

import (
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// writeFileAtomic 的错误里带着随机的临时文件名（bin/.a-ui-geo.dat.<随机>.tmp）。改动前降级原因原样带着它，
// 生成指纹里有原因，于是同一个写入故障每一轮都像一次新的生成：每 10 秒置一次重启标志、整份配置重算一遍、
// 一轮一条警告——磁盘满或 bin/ 只读时会一直这样转下去。
func TestFallbackReasonIsStableAcrossTempPaths(t *testing.T) {
	cleanupGeoDat(t)
	tag := regionTag([]string{"江苏省"})
	oldCode := geoDatCode(tag, []string{"192.0.2.0/24"})
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: oldCode, CIDRs: []string{"192.0.2.0/24"}}}); err != nil {
		t.Fatal(err)
	}
	newCode := geoDatCode(tag, []string{"192.0.2.0/24", "203.0.113.0/24"})
	mk := func() *geoPlan {
		return &geoPlan{TagByInbound: map[int]string{1: newCode}, TagOfCode: map[string]string{newCode: tag},
			FailClosed: map[int]string{}, Degraded: map[int]GeoDegradation{}}
	}
	a, b := mk(), mk()
	fallbackToExistingDat(a, &fs.PathError{Op: "open", Path: "bin/.a-ui-geo.dat.3619492071.tmp", Err: syscall.EACCES})
	fallbackToExistingDat(b, &fs.PathError{Op: "open", Path: "bin/.a-ui-geo.dat.8812345678.tmp", Err: syscall.EACCES})
	ra, rb := a.Degraded[1].Reason, b.Degraded[1].Reason
	if ra == "" || ra != rb {
		t.Fatalf("同一个写入故障两轮的降级原因不同：%q / %q", ra, rb)
	}
	if strings.Contains(ra, ".tmp") {
		t.Errorf("降级原因里带着临时文件名：%q", ra)
	}
	if !sameGeoGeneration(geoGeneration{Degraded: a.Degraded}, geoGeneration{Degraded: b.Degraded}) {
		t.Error("两轮生成被判为不同：会每轮都置一次重启标志")
	}
	// 管理员要能看出是什么问题：权限 / 空间这类常见原因要写进去。
	if !strings.Contains(ra, "权限") {
		t.Errorf("没有写权限的原因没有说出来：%q", ra)
	}
}
