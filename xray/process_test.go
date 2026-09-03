package xray

import (
	"testing"

	statsservice "github.com/xtls/xray-core/app/stats/command"
)

// xray 的 stats 里不止 inbound/outbound 两类条目：policy 开启用户级统计时会产生
// user>>>xxx>>>traffic>>>uplink，与 x-ui 等同源面板共用同一个 api 端口时读到的
// 统计里也会混入这类条目。它们不是错误，必须跳过。
//
// 这条测试守的是「解析不能 panic」：GetTraffic 由 XrayTrafficJob 在 cron 里调用，
// 而 cron 未配 Recover，一次 panic 会带走整个面板进程——现象是面板启动约 25 秒后
// 静默退出，systemd 只报 status=2。
func TestParseTrafficsSkipsUnrecognizedStatNames(t *testing.T) {
	stats := []*statsservice.Stat{
		{Name: "inbound>>>inbound-2652>>>traffic>>>uplink", Value: 11},
		{Name: "inbound>>>inbound-2652>>>traffic>>>downlink", Value: 22},
		{Name: "user>>>someone@xray.com>>>traffic>>>uplink", Value: 33},
		{Name: "user>>>someone@xray.com>>>traffic>>>downlink", Value: 44},
		{Name: "user>>>p59GukPGeP>>>traffic>>>uplink", Value: 55},
		{Name: "inbound>>>api>>>traffic>>>uplink", Value: 66},
		{Name: "inbound>>>api>>>traffic>>>downlink", Value: 77},
		{Name: "outbound>>>direct>>>traffic>>>downlink", Value: 88},
		{Name: "", Value: 99},
		{Name: "完全无法识别的条目", Value: 100},
	}

	traffics := parseTraffics(stats)

	// api 入站按既有行为排除，user>>> 与无法识别的条目跳过，只剩这两条
	if len(traffics) != 2 {
		t.Fatalf("期望解析出 2 条流量，实际 %d 条: %+v", len(traffics), traffics)
	}

	byTag := map[string]*Traffic{}
	for _, tf := range traffics {
		byTag[tf.Tag] = tf
	}

	in, ok := byTag["inbound-2652"]
	if !ok {
		t.Fatalf("缺少 inbound-2652: %+v", traffics)
	}
	if !in.IsInbound || in.Up != 11 || in.Down != 22 {
		t.Errorf("inbound-2652 解析错误: %+v", in)
	}

	out, ok := byTag["direct"]
	if !ok {
		t.Fatalf("缺少 direct: %+v", traffics)
	}
	if out.IsInbound || out.Down != 88 {
		t.Errorf("direct 解析错误: %+v", out)
	}
}

// 保持原有顺序语义：结果按条目首次出现的顺序排列，不依赖 map 遍历
func TestParseTrafficsKeepsFirstSeenOrder(t *testing.T) {
	stats := []*statsservice.Stat{
		{Name: "inbound>>>b>>>traffic>>>uplink", Value: 1},
		{Name: "inbound>>>a>>>traffic>>>uplink", Value: 2},
		{Name: "inbound>>>b>>>traffic>>>downlink", Value: 3},
	}

	traffics := parseTraffics(stats)

	if len(traffics) != 2 || traffics[0].Tag != "b" || traffics[1].Tag != "a" {
		t.Fatalf("顺序不符合预期: %+v", traffics)
	}
}
