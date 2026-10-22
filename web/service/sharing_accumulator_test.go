package service

import (
	"fmt"
	"testing"
	"time"

	"a-ui/database/model"
)

// provinceMeta 造一份「中国某省」的画像快照。
//
// 带上 Country 而不是只设 Province：生产采集走 selectNetworkMeta，它对中国段
// 必然同时给出这两个字段，测试输入不该是一个生产上造不出来的形状。
func provinceMeta(province string) NetworkMeta {
	if province == "" {
		return NetworkMeta{}
	}
	return NetworkMeta{Country: "中国", Province: province}
}

func oneObs() []sharingObservation {
	return []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: provinceMeta("江苏")}}
}

// 入站端口在公网上会被扫。若每个建立过连接的来源都落一行，一次端口扫描
// 就能在一小时内塞进几千行。60 秒门槛把一次性探测挡在外面。
func TestAccumulatorRequiresThresholdBeforeFlush(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	if got := a.observe(base, oneObs(), 30); len(got) != 0 {
		t.Fatalf("累计 30 秒就落库了: %+v", got)
	}
	got := a.observe(base.Add(30*time.Second), oneObs(), 30)
	if len(got) != 1 || got[0].ActiveSeconds != 60 {
		t.Fatalf("累计 60 秒 = %+v, want 1 条 ActiveSeconds=60", got)
	}
	if got := a.observe(base.Add(60*time.Second), oneObs(), 30); len(got) != 0 {
		t.Fatalf("距上次落库只多 30 秒，不该再落库: %+v", got)
	}
	got = a.observe(base.Add(90*time.Second), oneObs(), 30)
	if len(got) != 1 || got[0].ActiveSeconds != 120 {
		t.Fatalf("累计 120 秒 = %+v, want 1 条 ActiveSeconds=120", got)
	}
}

// 不满门槛的余量跨小时直接丢弃，不结转——结转会让一个每小时只用 30 秒的
// 扫描器攒几轮就绕开门槛。
func TestAccumulatorDiscardsSubThresholdRemainderAcrossHours(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 59, 30, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	if got := a.observe(h10, oneObs(), 30); len(got) != 0 {
		t.Fatalf("不该落库: %+v", got)
	}
	if got := a.observe(h11, oneObs(), 30); len(got) != 0 {
		t.Fatalf("跨小时结转了不满门槛的余量: %+v", got)
	}
}

// 已过门槛但最后一段还没落库的，跨小时时要补一次收尾写入，否则那段时长
// 永远丢失。
func TestAccumulatorFlushesCompletedHourOnRollover(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	a.observe(h10, oneObs(), 30)                     // 30
	a.observe(h10.Add(30*time.Second), oneObs(), 30) // 60，落库
	a.observe(h10.Add(60*time.Second), oneObs(), 30) // 90，未落库

	got := a.observe(h11, oneObs(), 30)
	if len(got) != 1 {
		t.Fatalf("跨小时 = %+v, want 1 条收尾记录", got)
	}
	if got[0].HourStart != model.AlignHourUTC(h10) {
		t.Errorf("收尾记录落在 %v, want 上一小时 %v", got[0].HourStart, model.AlignHourUTC(h10))
	}
	if got[0].ActiveSeconds != 90 {
		t.Errorf("收尾记录 ActiveSeconds = %v, want 90", got[0].ActiveSeconds)
	}
}

// 上限不是为正常场景设的（正常一小时 2~3 行），是为被针对性刷时让表的
// 大小有确定天花板。
func TestAccumulatorCapsRowsPerInboundPerHour(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	obs := make([]sharingObservation, 0, sharingMaxRowsPerHour+10)
	for i := 0; i < sharingMaxRowsPerHour+10; i++ {
		obs = append(obs, sharingObservation{
			InboundId: 1,
			IP:        fmt.Sprintf("10.0.%v.%v", i/256, i%256),
			Meta:      provinceMeta("江苏"),
		})
	}
	a.observe(base, obs, 30)
	got := a.observe(base.Add(30*time.Second), obs, 30)
	if len(got) != sharingMaxRowsPerHour {
		t.Errorf("落库 %v 条, want 上限 %v 条", len(got), sharingMaxRowsPerHour)
	}
}

// 上限只挡新来源，已在累计的继续累计——否则一次扫描就能把真实用户从
// 表里挤掉。
func TestAccumulatorCapDoesNotEvictExistingSources(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	real := sharingObservation{InboundId: 1, IP: "1.1.1.1", Meta: provinceMeta("江苏")}

	a.observe(base, []sharingObservation{real}, 30)

	flood := []sharingObservation{real}
	for i := 0; i < sharingMaxRowsPerHour+10; i++ {
		flood = append(flood, sharingObservation{
			InboundId: 1,
			IP:        fmt.Sprintf("10.0.%v.%v", i/256, i%256),
			Meta:      provinceMeta("江苏"),
		})
	}
	got := a.observe(base.Add(30*time.Second), flood, 30)

	found := false
	for _, f := range got {
		if f.IP == real.IP && f.ActiveSeconds == 60 {
			found = true
		}
	}
	if !found {
		t.Error("真实用户被扫描流量挤掉了")
	}
}

// 删除入站必须连内存累计一起清掉。只删库的话，rolloverLocked 会在下一次
// 跨小时把残量重新 upsert 回去，而 SQLite 复用自增 id 会让这批复活的行
// 绑到下一个建出来的入站上——那时引用不再悬空，孤儿兜底也拦不住。
func TestAccumulatorForgetDropsInboundBeforeRollover(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)
	obs := []sharingObservation{
		{InboundId: 1, IP: "1.1.1.1", Meta: provinceMeta("江苏")},
		{InboundId: 2, IP: "2.2.2.2", Meta: provinceMeta("上海")},
	}

	// 攒到 90 秒：60 秒时落过一次，剩下的 30 秒还挂在内存里。
	a.observe(h10, obs, 30)
	a.observe(h10.Add(30*time.Second), obs, 30)
	a.observe(h10.Add(60*time.Second), obs, 30)

	a.forget(1)

	got := a.observe(h11, nil, 30)
	for _, f := range got {
		if f.InboundId == 1 {
			t.Errorf("已删除的入站 1 在跨小时时又被写回: %+v", f)
		}
	}
	// 没被 forget 的入站 2 仍应正常收尾，证明 forget 没有误伤别人。
	found := false
	for _, f := range got {
		if f.InboundId == 2 && f.ActiveSeconds == 90 {
			found = true
		}
	}
	if !found {
		t.Errorf("入站 2 的收尾写入被误删了, got %+v", got)
	}
}

// 字节量是并存判定的「实质使用」判据，累加器必须把它攒出来。
//
// sharingObservation.Up/Down 是自上一轮以来的**增量**（tracker 的
// drainPending），累加器直接相加。首轮的增量同样计入：它就是这 30 秒里
// 真实产生的字节，不再像改用增量之前那样只能拿来当基线。
func TestAccumulatorAccumulatesByteDeltas(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	obs := func(up, down int64) []sharingObservation {
		return []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: provinceMeta("江苏"), Up: up, Down: down}}
	}

	a.observe(base, obs(1000, 2000), 30)
	got := a.observe(base.Add(30*time.Second), obs(500, 1500), 30)
	if len(got) != 1 {
		t.Fatalf("满 60 秒没落库: %+v", got)
	}
	if got[0].ActiveUp != 1500 || got[0].ActiveDown != 3500 || got[0].ActiveBytes != 5000 {
		t.Errorf("落库 up/down/bytes = %d/%d/%d, want 1500/3500/5000",
			got[0].ActiveUp, got[0].ActiveDown, got[0].ActiveBytes)
	}
	if got[0].ActiveSeconds != 60 {
		t.Errorf("ActiveSeconds = %d, want 60", got[0].ActiveSeconds)
	}
}

// 跨小时收尾写入同样要带上字节数，否则整点前最后一段的流量在库里是 0，
// 而 computeCoexist 会把 0 字节的行判成「不实质」直接丢掉。
func TestAccumulatorCarriesBytesOnRollover(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)
	obs := func(up int64) []sharingObservation {
		return []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: provinceMeta("江苏"), Up: up}}
	}

	a.observe(h10, obs(0), 30)
	a.observe(h10.Add(30*time.Second), obs(5000), 30) // 落库，5000 字节
	a.observe(h10.Add(60*time.Second), obs(4000), 30) // 未到下一次落库门槛
	got := a.observe(h11, obs(0), 30)
	if len(got) == 0 {
		t.Fatal("跨小时没有补写收尾记录")
	}
	if got[0].HourStart != model.AlignHourUTC(h10) {
		t.Fatalf("收尾记录落在了错误的小时: %+v", got[0])
	}
	if got[0].ActiveBytes != 9000 {
		t.Errorf("收尾记录 ActiveBytes = %d, want 9000", got[0].ActiveBytes)
	}
}

func bytesOnly(ip string, up int64) sharingObservation {
	return sharingObservation{InboundId: 1, IP: ip, Meta: provinceMeta("江苏"), Up: up, BytesOnly: true}
}

// 只有字节、本轮没被观测为活跃的来源（短连接在采样那一刻恰好都已关闭，或
// 连接还在但被判闲置）：不满实质使用门槛时不落库，也不占行数上限——否则
// 一次扫描里每个探测包都能抢走一个名额，把真实用户挤出这一小时。
func TestAccumulatorBytesOnlyBelowGateIsNotWritten(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	var flood []sharingObservation
	for i := 0; i < sharingMaxRowsPerHour+10; i++ {
		flood = append(flood, bytesOnly(fmt.Sprintf("10.0.%v.%v", i/256, i%256), 600))
	}
	if got := a.observe(base, flood, 30); len(got) != 0 {
		t.Fatalf("不满门槛的纯字节来源落库了: %+v", got)
	}

	real := sharingObservation{InboundId: 1, IP: "1.1.1.1", Meta: provinceMeta("江苏")}
	a.observe(base.Add(30*time.Second), []sharingObservation{real}, 30)
	got := a.observe(base.Add(60*time.Second), []sharingObservation{real}, 30)
	if len(got) != 1 || got[0].IP != real.IP {
		t.Errorf("真实用户被纯字节来源挤出了上限: %+v", got)
	}
}

// 纯字节来源攒满实质使用门槛（coexistMinActiveBytes）就落库，活跃时长记 0。
// 这正是改动前整段丢失的那部分：短连接为主的客户端在 30 秒一次的观测时刻
// 往往一条连接都没有，按秒计永远到不了 60 秒门槛。
func TestAccumulatorBytesOnlyIsWrittenOnceOverGate(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	half := int64(coexistMinActiveBytes / 2)

	if got := a.observe(base, []sharingObservation{bytesOnly("1.1.1.1", half)}, 30); len(got) != 0 {
		t.Fatalf("半个门槛就落库了: %+v", got)
	}
	got := a.observe(base.Add(30*time.Second), []sharingObservation{bytesOnly("1.1.1.1", half)}, 30)
	if len(got) != 1 {
		t.Fatalf("攒满门槛没落库: %+v", got)
	}
	if got[0].ActiveUp != 2*half || got[0].ActiveSeconds != 0 {
		t.Errorf("落库 = %+v, want ActiveUp=%d ActiveSeconds=0", got[0], 2*half)
	}
}

// 先以纯字节出现、随后被观测为活跃的来源：之前攒下的字节并进它的 cell，
// 不能因为换了一条路径就丢掉。
func TestAccumulatorAbsorbsLooseBytesWhenObserved(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	active := func(up int64) []sharingObservation {
		return []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: provinceMeta("江苏"), Up: up}}
	}

	a.observe(base, []sharingObservation{bytesOnly("1.1.1.1", 700)}, 30)
	a.observe(base.Add(30*time.Second), active(100), 30)
	got := a.observe(base.Add(60*time.Second), active(200), 30)
	if len(got) != 1 || got[0].ActiveUp != 1000 {
		t.Errorf("落库 = %+v, want ActiveUp=1000（纯字节 700 + 活跃 300）", got)
	}
}

// 已经落过库的 cell 之后只收到纯字节（来源还在用、只是观测时刻没有连接）：
// 字节有变化就要再写，否则这部分流量要等到整点收尾才进库，期间查询看到
// 的是旧值；但也不能每轮都写，与按秒落库一样至少间隔 sharingFlushThreshold 秒。
func TestAccumulatorRewritesBytesOnlyProgress(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	active := []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: provinceMeta("江苏")}}

	a.observe(base, active, 30)
	if got := a.observe(base.Add(30*time.Second), active, 30); len(got) != 1 {
		t.Fatalf("满 60 秒没落库: %+v", got)
	}
	if got := a.observe(base.Add(60*time.Second), []sharingObservation{bytesOnly("1.1.1.1", 4000)}, 30); len(got) != 0 {
		t.Fatalf("距上次落库只过了 30 秒就又写了: %+v", got)
	}
	got := a.observe(base.Add(90*time.Second), []sharingObservation{bytesOnly("1.1.1.1", 1000)}, 30)
	if len(got) != 1 || got[0].ActiveUp != 5000 || got[0].ActiveSeconds != 60 {
		t.Errorf("落库 = %+v, want ActiveUp=5000 ActiveSeconds=60（纯字节不加时长）", got)
	}
}

// 纯字节只停留在内存、不满门槛的余量，跨小时同样直接丢弃。
func TestAccumulatorDropsLooseBytesOnRollover(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 59, 30, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)
	half := int64(coexistMinActiveBytes / 2)

	a.observe(h10, []sharingObservation{bytesOnly("1.1.1.1", half)}, 30)
	if got := a.observe(h11, []sharingObservation{bytesOnly("1.1.1.1", half)}, 30); len(got) != 0 {
		t.Errorf("上一小时的余量结转进了新的一小时: %+v", got)
	}
}

// 画像必须整组替换，不能逐字段填。逐字段填会在归属地库更新的那一刻拼出
// 「旧省份 + 新城市」这种跨源组合，而画像键正是按这一组字段拼的。
func TestAccumulatorReplacesMetaAsAWhole(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	old := NetworkMeta{Country: "中国", Province: "北京市", City: "北京市", ISP: "中国移动"}
	fresh := NetworkMeta{Country: "中国", Province: "湖北省", City: "武汉市", ISP: "中国联通"}

	a.observe(base, []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: old}}, 30)
	got := a.observe(base.Add(30*time.Second),
		[]sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: fresh}}, 30)

	if len(got) != 1 {
		t.Fatalf("落库 %v 条, want 1", len(got))
	}
	if got[0].Meta != fresh {
		t.Errorf("Meta = %+v, want 整组换成 %+v（逐字段填会留下旧省份配新城市）", got[0].Meta, fresh)
	}
}

// 查不到归属地时不能覆盖已经判定出来的画像：一次查库失败（库正在热替换、
// 或这一轮恰好没加载完）不该把已知信息抹掉。
func TestAccumulatorKeepsKnownMetaWhenLookupYieldsNothing(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	known := NetworkMeta{Country: "中国", Province: "江苏省", City: "南通市", ISP: "中国电信"}

	a.observe(base, []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: known}}, 30)
	got := a.observe(base.Add(30*time.Second),
		[]sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: NetworkMeta{}}}, 30)

	if len(got) != 1 {
		t.Fatalf("落库 %v 条, want 1", len(got))
	}
	if got[0].Meta != known {
		t.Errorf("Meta = %+v, want 保留 %+v", got[0].Meta, known)
	}
}

// 境外来源只有 Country（ipdb.normalize 对非中国段丢掉省市），它必须能更新
// 画像。判据若写成「Province 非空才更新」，境外来源的画像会永远停在零值。
func TestAccumulatorAcceptsForeignMetaWithoutProvince(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	foreign := NetworkMeta{Country: "美国", ISP: "Amazon"}

	a.observe(base, []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: NetworkMeta{}}}, 30)
	got := a.observe(base.Add(30*time.Second),
		[]sharingObservation{{InboundId: 1, IP: "1.1.1.1", Meta: foreign}}, 30)

	if len(got) != 1 {
		t.Fatalf("落库 %v 条, want 1", len(got))
	}
	if got[0].Meta != foreign {
		t.Errorf("Meta = %+v, want %+v", got[0].Meta, foreign)
	}
}

// 暂存表满（一次扫描留下了上千个小来源）时，单次就超过门槛的新来源仍然要
// 落库：先判门槛、再判暂存是否有空位。顺序反过来的话，扫描期间整个小时里
// 所有新的短连接用户都会被静默丢掉。
func TestAccumulatorPromotesOverGateEvenWhenLooseIsFull(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	var flood []sharingObservation
	for i := 0; i < sharingMaxLoosePerHour; i++ {
		flood = append(flood, bytesOnly(fmt.Sprintf("10.%v.%v.%v", i>>16&255, i>>8&255, i&255), 100))
	}
	a.observe(base, flood, 30)

	got := a.observe(base.Add(30*time.Second), []sharingObservation{bytesOnly("1.1.1.1", coexistMinActiveBytes)}, 30)
	if len(got) != 1 || got[0].IP != "1.1.1.1" {
		t.Errorf("暂存表满时，满门槛的新来源被丢掉了: %+v", got)
	}
}
