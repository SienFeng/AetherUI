package controller

import (
	"testing"
)

// 新接口必须真的挂上路由并返回结构完整的响应体。
//
// 打真实 HTTP 请求而不是直接调 service：这条测试要守的正是「路由注册了、
// 入参绑定上了、service 接上了」这三件 controller 自己的事，直接调 service
// 一件都守不到。
func TestIPUsageEndpointReturnsEntriesShape(t *testing.T) {
	r := newTrafficRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/aui/inbound/ipUsage/"+itoa(in.Id), "range=today")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if _, ok := obj["entries"]; !ok {
		t.Errorf("响应里没有 entries 字段: %v", obj)
	}
	if _, ok := obj["split"]; !ok {
		t.Errorf("响应里没有 split 字段: %v", obj)
	}
	if _, ok := obj["beyondRetention"]; !ok {
		t.Errorf("响应里没有 beyondRetention 字段: %v", obj)
	}
}

// range=1y 必须整块降级：按来源 IP 的明细只保留 30 天。
//
// 这条同时守住了「start/end 之外的 range 也真的被绑定进来了」——若绑定
// 标签写错，range 永远是空串、永远回落今日，beyondRetention 就永远是 false。
func TestIPUsageEndpointDegradesBeyondRetention(t *testing.T) {
	r := newTrafficRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/aui/inbound/ipUsage/"+itoa(in.Id), "range=1y")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if obj["beyondRetention"] != true {
		t.Errorf("beyondRetention = %v，期望 true（1 年超出 30 天保留期）", obj["beyondRetention"])
	}
	if obj["reason"] == "" {
		t.Error("reason 为空——「看不到」必须和「没有」能区分开")
	}
}

// 自定义区间的三个入参必须一起绑定上。
//
// 只绑 range 不绑 start/end 的话，custom 会因日期不可解析而回落「今日」，
// 而接口照常返回 success——没有任何一层会报错。这条用一个明确超出 30 天的
// 区间把它逼出来：绑定成功则 beyondRetention 为 true，绑丢了则为 false。
func TestIPUsageEndpointBindsCustomStartAndEnd(t *testing.T) {
	r := newTrafficRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/aui/inbound/ipUsage/"+itoa(in.Id),
		"range=custom&start=2020-01-01&end=2020-12-31")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if obj["beyondRetention"] != true {
		t.Errorf("beyondRetention = %v，期望 true——start/end 没有被绑定进来，"+
			"custom 回落成了「今日」", obj["beyondRetention"])
	}
}

// 越界入参一律钳制后照常返回，不得让接口失败。
func TestIPUsageEndpointClampsInsteadOfFailing(t *testing.T) {
	r := newTrafficRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	for _, body := range []string{
		"range=custom&start=垃圾&end=更多垃圾",
		"range=custom&start=2000-01-01&end=2026-09-09",
		"range=不存在的档位",
		"range=custom&start=2026-09-09&end=2026-09-01",
		"",
	} {
		msg := postForm(t, r, "/aui/inbound/ipUsage/"+itoa(in.Id), body)
		if !msg.Success {
			t.Errorf("body=%q 时 success = false, msg = %q（越界入参应当钳制而非报错）",
				body, msg.Msg)
		}
	}
}

// id 不是数字时必须失败——解析要排在表单绑定之前。
func TestIPUsageEndpointRejectsNonNumericId(t *testing.T) {
	r := newTrafficRouter(t)

	msg := postForm(t, r, "/aui/inbound/ipUsage/abc", "range=today")
	if msg.Success {
		t.Error("id 不是数字时应当失败")
	}
}
