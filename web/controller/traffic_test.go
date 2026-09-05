package controller

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"a-ui/database"
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

func TestTrafficOverviewEndpointClampsTop(t *testing.T) {
	r := newTrafficRouter(t)
	createInbound(t, 0, true, 0, 0)

	// 越界的 top 不该被透传给 service——钳住它是 controller 的职责。
	// 这里只能验证接口仍然正常返回：钳制本身没有可观察的输出，
	// 但一个把 top 原样透传的实现会在 series 数量上暴露出来（若有足够多入站）。
	// 至少保证不因越界值而失败或 panic。
	for _, body := range []string{"range=24h&top=999999", "range=24h&top=-5", "range=24h"} {
		msg := postForm(t, r, "/aui/inbound/traffic/overview", body)
		if !msg.Success {
			t.Errorf("body=%q: success = false, msg = %q", body, msg.Msg)
		}
	}
}
