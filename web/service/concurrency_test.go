package service

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/netdiag"
)

func entry(ip string, firstSeenSec int64) OnlineIP {
	return OnlineIP{IP: ip, FirstSeen: time.Unix(firstSeenSec, 0).UnixMilli(), Conns: 1}
}

func TestSelectOverQuotaAllowsEveryoneWhenUnlimited(t *testing.T) {
	list := []OnlineIP{entry("1.1.1.1", 100), entry("2.2.2.2", 200)}
	if over := selectOverQuota(list, 0); len(over) != 0 {
		t.Errorf("limit=0 表示不限制，不应踢人，实际踢了 %v", over)
	}
}

func TestSelectOverQuotaAllowsWhenUnderLimit(t *testing.T) {
	list := []OnlineIP{entry("1.1.1.1", 100), entry("2.2.2.2", 200)}
	if over := selectOverQuota(list, 2); len(over) != 0 {
		t.Errorf("在线数正好等于额度，不应踢人，实际踢了 %v", over)
	}
}

func TestSelectOverQuotaRejectsLatecomers(t *testing.T) {
	// 先来后到：额度 1，甲先上线，乙丙都应被拒。
	list := []OnlineIP{
		entry("3.3.3.3", 300),
		entry("1.1.1.1", 100),
		entry("2.2.2.2", 200),
	}
	over := selectOverQuota(list, 1)
	if len(over) != 2 {
		t.Fatalf("被拒数 = %d，期望 2：%v", len(over), over)
	}
	if over[0] != "2.2.2.2" || over[1] != "3.3.3.3" {
		t.Errorf("被拒的是 %v，期望保留最早上线的 1.1.1.1，拒绝 2.2.2.2 与 3.3.3.3", over)
	}
}

func TestSelectOverQuotaIsDeterministicOnTiedFirstSeen(t *testing.T) {
	// 面板刚重启时所有 IP 的首次观测时间相同，此时必须有稳定的次序，
	// 否则每一轮踢的人都不一样，谁也用不成。
	list := []OnlineIP{
		entry("10.0.0.3", 100),
		entry("10.0.0.1", 100),
		entry("10.0.0.2", 100),
	}
	first := selectOverQuota(list, 1)
	for i := 0; i < 5; i++ {
		if got := selectOverQuota(list, 1); len(got) != len(first) || got[0] != first[0] || got[1] != first[1] {
			t.Fatalf("第 %d 次结果 %v 与首次 %v 不一致", i, got, first)
		}
	}
	if first[0] != "10.0.0.2" || first[1] != "10.0.0.3" {
		t.Errorf("被拒的是 %v，期望按 IP 升序保留 10.0.0.1", first)
	}
}

func TestTrackerMarksRejectedIPsAsBlocked(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)
	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.1.1.1", 10, 10),
		conn(2, testPort, "2.2.2.2", 10, 10),
	}, testPorts, now)

	tk.setRejected(testPort, []string{"2.2.2.2"}, now)

	list := tk.snapshot(testPort, noLocation)
	if len(list) != 2 {
		t.Fatalf("条目数 = %d，期望 2", len(list))
	}
	byIP := map[string]OnlineIP{}
	for _, e := range list {
		byIP[e.IP] = e
	}
	if byIP["1.1.1.1"].Blocked {
		t.Error("1.1.1.1 没超额，不该标记为被拒")
	}
	if !byIP["2.2.2.2"].Blocked {
		t.Error("2.2.2.2 超额被拒，必须标记出来，否则管理员不知道谁连不上")
	}
	if byIP["2.2.2.2"].RejectedAt != now.UnixMilli() {
		t.Errorf("RejectedAt = %d，期望 %d", byIP["2.2.2.2"].RejectedAt, now.UnixMilli())
	}
}

func TestTrackerClearsBlockedWhenNoLongerOverQuota(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)
	tk.update([]netdiag.Conn{conn(1, testPort, "2.2.2.2", 10, 10)}, testPorts, now)
	tk.setRejected(testPort, []string{"2.2.2.2"}, now)

	// 下一轮额度腾出来了，同一个 IP 不再超额。
	tk.setRejected(testPort, nil, now.Add(time.Second))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.Blocked {
		t.Error("已不再超额的 IP 仍被标记为被拒，界面会一直显示红标")
	}
}

func TestTrackerKeepsRejectedIPVisibleAfterItsConnectionsDie(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)
	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.1.1.1", 10, 10),
		conn(2, testPort, "2.2.2.2", 10, 10),
	}, testPorts, now)
	tk.setRejected(testPort, []string{"2.2.2.2"}, now)

	// 被拒的 IP 连接已被断开，连接表里就没有它了；但管理员仍需要看到
	// "有人正在被拒绝"，否则超额这件事对他完全不可见。
	tk.update([]netdiag.Conn{conn(1, testPort, "1.1.1.1", 20, 20)}, testPorts, now.Add(time.Second))

	list := tk.snapshot(testPort, noLocation)
	if len(list) != 2 {
		t.Fatalf("条目数 = %d，期望 2（1 个在线 + 1 个被拒）：%+v", len(list), list)
	}
	var rejected *OnlineIP
	for i := range list {
		if list[i].IP == "2.2.2.2" {
			rejected = &list[i]
		}
	}
	if rejected == nil {
		t.Fatal("被拒的 2.2.2.2 从列表里消失了")
	}
	if rejected.Conns != 0 {
		t.Errorf("连接数 = %d，被拒 IP 已无连接，应为 0", rejected.Conns)
	}
	if !rejected.Blocked {
		t.Error("被拒 IP 必须带 Blocked 标记")
	}
}

func TestTrackerForgetsRejectionsAfterRetention(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)
	tk.update([]netdiag.Conn{conn(1, testPort, "2.2.2.2", 10, 10)}, testPorts, now)
	tk.setRejected(testPort, []string{"2.2.2.2"}, now)
	tk.update(nil, testPorts, now.Add(time.Second))

	// 超过保留期后，历史记录不该无限堆积。
	tk.update(nil, testPorts, now.Add(onlineRejectRetention+time.Minute))

	if list := tk.snapshot(testPort, noLocation); len(list) != 0 {
		t.Errorf("保留期过后仍有 %d 条记录：%+v", len(list), list)
	}
}

func TestUpdateInboundPersistsConcurrencyLimit(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s := InboundService{}
	in := &model.Inbound{
		UserId: 1, Port: 20011, Protocol: model.VLESS, Tag: "inbound-20011",
		Remark: "甲", Enable: true, Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
		ConcurrencyLimit: 0,
	}
	if err := s.AddInbound(in); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}

	in.ConcurrencyLimit = 3
	if err := s.UpdateInbound(in); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}

	// UpdateInbound 是逐字段复制的，漏掉新字段会让"改了保存后没生效"
	// 这种问题静默发生。
	got, err := s.GetInbound(in.Id)
	if err != nil {
		t.Fatalf("GetInbound: %v", err)
	}
	if got.ConcurrencyLimit != 3 {
		t.Errorf("ConcurrencyLimit = %d，期望 3", got.ConcurrencyLimit)
	}
}

func TestConcurrencyLimitedInboundsOnlyReturnsEnabledOnesWithLimit(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	mk := func(port, limit int, enable bool) {
		in := &model.Inbound{
			UserId: 1, Port: port, Protocol: model.VLESS, Tag: "inbound-" + strconv.Itoa(port),
			Enable: enable, Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
			ConcurrencyLimit: limit,
		}
		if err := db.Create(in).Error; err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	mk(30001, 2, true)  // 要管
	mk(30002, 0, true)  // 没设限制
	mk(30003, 5, false) // 已停用，xray 里根本没这个入站

	s := ConcurrencyService{}
	got, err := s.limitedInbounds(time.Now())
	if err != nil {
		t.Fatalf("limitedInbounds: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("命中 %d 个入站，期望 1：%+v", len(got), got)
	}
	if got[0].Port != 30001 {
		t.Errorf("命中端口 %d，期望 30001", got[0].Port)
	}
}

func TestEnforceDoesNothingWithoutAnyLimitedInbound(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	in := &model.Inbound{
		UserId: 1, Port: 31001, Protocol: model.VLESS, Tag: "inbound-31001",
		Enable: true, Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
		ConcurrencyLimit: 0,
	}
	if err := db.Create(in).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	// 没有任何入站设了额度时必须直接返回，一次系统调用都不做。
	// 开发机不是 Linux：真去读连接表会返回 ErrUnsupported，
	// 拿到 nil 就证明确实短路了。
	if err := (&ConcurrencyService{}).Enforce(); err != nil {
		t.Errorf("Enforce = %v，没有受限入站时应当直接返回 nil 而不去碰内核", err)
	}
}

func TestLiveOnlyDropsRejectedGhostEntries(t *testing.T) {
	// 被拒的 IP 连接已断、只作为历史条目留在列表里。若把它也算进在线数，
	// 它会永久占着一个名额——额度 1 时会变成谁都连不上，而且自愈不了。
	list := []OnlineIP{
		{IP: "1.1.1.1", Conns: 2},
		{IP: "2.2.2.2", Conns: 0, Blocked: true},
	}
	live := liveOnly(list)
	if len(live) != 1 {
		t.Fatalf("在线条目数 = %d，期望 1：%+v", len(live), live)
	}
	if live[0].IP != "1.1.1.1" {
		t.Errorf("留下的是 %q，期望 1.1.1.1", live[0].IP)
	}
}

func TestPlanRejectionsDoesNotLetGhostEntryOccupyTheQuota(t *testing.T) {
	// 关键的失效场景：1.1.1.1 曾被拒、连接已断，只作为历史条目留在列表里
	// （上线时间更早）；2.2.2.2 是当前唯一真实在线的人。
	// 若判定时不先剔除历史条目，额度 1 会被那个"幽灵"占住，真实用户
	// 反而被永久踢下线，而且这个状态自己好不了。
	list := []OnlineIP{
		{IP: "1.1.1.1", Conns: 0, Blocked: true, FirstSeen: 100},
		{IP: "2.2.2.2", Conns: 1, FirstSeen: 200},
	}
	if over := planRejections(list, 1); len(over) != 0 {
		t.Errorf("被拒的是 %v，唯一真实在线的用户不该被踢", over)
	}
}
