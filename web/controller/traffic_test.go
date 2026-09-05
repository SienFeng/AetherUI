package controller

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"a-ui/database"
	"a-ui/database/model"
)

// newTrafficRouter 在 newRenewRouter 的基础上再开一个用量库。
// newRenewRouter 只开主库，用量接口在那种状态下走的是「库不可用」分支。
func newTrafficRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r := newRenewRouter(t)
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
	return r
}

func decodeTrafficObj(t *testing.T, obj interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("重新编码响应体: %v", err)
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应体 %q: %v", raw, err)
	}
	return out
}

func TestTrafficHistoryEndpointBindsRangeFromUrlencodedForm(t *testing.T) {
	r := newTrafficRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	// 前端发的是 urlencoded，绑定标签必须是 form 而不是 json，
	// 否则 range 永远是空串、图永远只显示 24 小时那一档。
	msg := postForm(t, r, "/aui/inbound/traffic/history/"+itoa(in.Id), "range=1y")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if got := obj["granularity"]; got != "day" {
		t.Errorf("granularity = %v，期望 day（range=1y 没有被绑定进来）", got)
	}
	points, ok := obj["points"].([]interface{})
	if !ok {
		t.Fatalf("响应里没有 points 数组: %v", obj)
	}
	if len(points) != 365 {
		t.Errorf("点数 = %d，期望 365", len(points))
	}
}

func TestTrafficHistoryEndpointDefaultsWithoutRange(t *testing.T) {
	r := newTrafficRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/aui/inbound/traffic/history/"+itoa(in.Id), "")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if got := obj["granularity"]; got != "hour" {
		t.Errorf("granularity = %v，期望回落到 hour", got)
	}
}

func TestTrafficHistoryEndpointRejectsNonNumericId(t *testing.T) {
	r := newTrafficRouter(t)

	msg := postForm(t, r, "/aui/inbound/traffic/history/abc", "range=24h")
	if msg.Success {
		t.Error("id 不是数字时应当失败")
	}
}

func TestTrafficOverviewEndpointReturnsLabelsAndSeries(t *testing.T) {
	r := newTrafficRouter(t)
	createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/aui/inbound/traffic/overview", "range=24h&top=12")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	labels, ok := obj["labels"].([]interface{})
	if !ok || len(labels) != 24 {
		t.Fatalf("labels = %v，期望 24 个", obj["labels"])
	}
	// 没有任何用量时 series 必须是空数组而不是 null：
	// 前端会对它做 .map，null 会直接报 TypeError 把整张图打掉。
	if _, ok := obj["series"].([]interface{}); !ok {
		t.Errorf("series = %v，期望空数组而不是 null", obj["series"])
	}
}

func TestTrafficOverviewEndpointSurvivesUnavailableDatabase(t *testing.T) {
	r := newTrafficRouter(t)
	createInbound(t, 0, true, 0, 0)
	database.ResetTrafficDBForTest()

	msg := postForm(t, r, "/aui/inbound/traffic/overview", "range=24h")
	// 库不可用是一种要如实告知的状态，不是接口错误：
	// 报 500 会让整个系统状态页看起来是坏的。
	if !msg.Success {
		t.Fatalf("库不可用时接口不该失败, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if obj["reason"] == "" {
		t.Error("库不可用时必须给出 reason")
	}
}

// createInboundWithTraffic 建一个入站并在它当前小时的桶里写入指定用量。
//
// createInbound（inbound_renew_test.go）的端口写死在 10011，建多个会撞
// port/tag 的 unique 约束，这里必须自己错开端口。amount 互不相同是为了让
// Top N 的排序有确定结果，不依赖数据库返回顺序。
func createInboundWithTraffic(t *testing.T, port int, remark string, amount int64) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Tag: fmt.Sprintf("inbound-%d", port),
		Remark: remark, Enable: true,
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
	}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("创建入站: %v", err)
	}
	// 面板默认时区（defaultValueMap["timeLocation"]），与 Overview 内部
	// GetTimeLocation 在测试环境下的回落值一致。
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	bucket := &model.TrafficBucket{
		Granularity: model.GranularityHour,
		InboundId:   in.Id,
		BucketStart: model.AlignHour(time.Now(), loc),
		Up:          amount,
	}
	if err := database.GetTrafficDB().Create(bucket).Error; err != nil {
		t.Fatalf("写入用量桶: %v", err)
	}
	return in
}

func TestTrafficOverviewEndpointClampsTop(t *testing.T) {
	r := newTrafficRouter(t)

	// 15 个入站，数量大于钳制回落值 12，用量互不相同（15 递减到 1）以获得
	// 确定的 Top N 排序。
	const n = 15
	for i := 0; i < n; i++ {
		createInboundWithTraffic(t, 20500+i, fmt.Sprintf("入站%d", i), int64(n-i))
	}

	// top=999999：没有钳制的话会原样传给 service 的 db.Limit(999999)，
	// 合法 SQL、不报错，15 个入站会被全部返回。钳制生效则回落到 12——
	// 这一条只有 controller 的钳制能满足，是本测试要证伪的核心。
	msg := postForm(t, r, "/aui/inbound/traffic/overview", "range=24h&top=999999")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	series, _ := obj["series"].([]interface{})
	if len(series) != 12 {
		t.Errorf("top=999999 时 series 长度 = %d，期望被钳到 12", len(series))
	}

	// 合法值必须原样透传：防的是把钳制写成无条件覆盖成 12，
	// 那样这里也会返回 12 而不是 3。
	msg = postForm(t, r, "/aui/inbound/traffic/overview", "range=24h&top=3")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj = decodeTrafficObj(t, msg.Obj)
	series, _ = obj["series"].([]interface{})
	if len(series) != 3 {
		t.Errorf("top=3 时 series 长度 = %d，期望原样透传为 3", len(series))
	}

	// 越界的负值与零值靠 service 层既有的 topN <= 0 → 12 回落，
	// 这里只保证不因此失败或 panic。
	for _, body := range []string{"range=24h&top=-5", "range=24h"} {
		msg := postForm(t, r, "/aui/inbound/traffic/overview", body)
		if !msg.Success {
			t.Errorf("body=%q: success = false, msg = %q", body, msg.Msg)
		}
	}
}
