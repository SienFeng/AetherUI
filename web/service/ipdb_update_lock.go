package service

import (
	"errors"
	"sync"
)

// ipdbUpdateMu 串行化所有会重建 IP 库的路径：设置页的「立即更新」（UpdateNow）、
// 每 10 分钟一次的定时更新（RunScheduledUpdate）、启动后的首次补拉
// （RunInitialUpdate）。
//
// 三条路径都会下载几十 MB、写同一个目标文件，再分别记下 ETag 与「上次向上游
// 确认的时间」。同时跑的话不只是白下一份：两次的 rename 与两次的 ETag/确认时间
// 写入互相交错，谁后写完谁说了算，记下的 ETag 可能对应的是另一份库，此后的
// 条件请求会被 304 挡回、旧库再也换不掉。IP2Location 还每 24 小时只放行 5 次
// 下载，并发的那一次是实打实的浪费。
//
// 包级变量而不是挂在 IPDBService 上：service 都是按值嵌入的空结构体，各处的
// IPDBService 是不同的实例。
var ipdbUpdateMu sync.Mutex

var errIPDBUpdateInProgress = errors.New("IP 库更新进行中，请稍后再试")

// beginIPDBUpdate 尝试占用更新锁，拿不到立即返回 false，从不排队：手动更新要
// 立刻告诉管理员「更新进行中」，后台任务跳过这一轮即可——定时任务 10 分钟后会
// 再判断一次，排队只会让它在别人刚更新完之后又下一遍。
func beginIPDBUpdate() (end func(), ok bool) {
	if !ipdbUpdateMu.TryLock() {
		return nil, false
	}
	return ipdbUpdateMu.Unlock, true
}
