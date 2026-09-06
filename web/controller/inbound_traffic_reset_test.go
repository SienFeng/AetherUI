package controller

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"a-ui/database"
	"a-ui/database/model"
)

// DisabledByTraffic 与 LastResetAt 是服务端状态，前端一个字节都不该能改：
//
//   - DisabledByTraffic 决定「下个重置周期要不要把这个入站拉回来」。能从请求体
//     改的话，一个被管理员手动停用的入站可以被伪造成「因超流量停用」，下个月
//     自己活过来。
//   - LastResetAt 决定「本周期清没清过」。能改的话，把它置 0 就能让下一轮任务
//     立刻清一次流量，绕过流量配额。
//
// 两者都靠模型上的 form:"-" 挡住（gin 1.7.1 binding/form_mapping.go:70 对
// 标签为 "-" 的字段直接跳过）。这条测试守的就是那两个标签——把 form:"-"
// 去掉、或改成 form:"disabledByTraffic"，绑定立刻就通了，而没有任何一层
// 会报错。
func TestInboundBindingIgnoresServerOwnedResetFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := strings.NewReader(
		"port=10011&protocol=vless&trafficResetMode=1" +
			"&disabledByTraffic=true&lastResetAt=1234567890")
	req := httptest.NewRequest("POST", "/", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	got := &model.Inbound{}
	if err := c.ShouldBind(got); err != nil {
		t.Fatalf("ShouldBind: %v", err)
	}

	// 先确认这个请求体本身是能被解析的，否则下面两条断言是空的。
	if got.TrafficResetMode != 1 {
		t.Fatalf("TrafficResetMode = %d, want 1（请求体没被解析，后面的断言无意义）",
			got.TrafficResetMode)
	}
	if got.DisabledByTraffic {
		t.Error("DisabledByTraffic 可以从请求体设置")
	}
	if got.LastResetAt != 0 {
		t.Errorf("LastResetAt = %d，可以从请求体设置", got.LastResetAt)
	}
}

// 同一条不变量的另一半。前端目前用 form 编码（axios-init.js 设了
// x-www-form-urlencoded），但那是一行配置的事；改成 JSON 提交之后
// form:"-" 就不管用了，挡住这一路的是 json:"-"。
func TestInboundJSONBindingIgnoresServerOwnedResetFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := strings.NewReader(
		`{"port":10011,"protocol":"vless","trafficResetMode":1,` +
			`"disabledByTraffic":true,"lastResetAt":1234567890}`)
	req := httptest.NewRequest("POST", "/", body)
	req.Header.Set("Content-Type", "application/json")

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	got := &model.Inbound{}
	if err := c.ShouldBind(got); err != nil {
		t.Fatalf("ShouldBind: %v", err)
	}

	if got.TrafficResetMode != 1 {
		t.Fatalf("TrafficResetMode = %d, want 1（请求体没被解析，后面的断言无意义）",
			got.TrafficResetMode)
	}
	if got.DisabledByTraffic {
		t.Error("DisabledByTraffic 可以从 JSON 请求体设置")
	}
	if got.LastResetAt != 0 {
		t.Errorf("LastResetAt = %d，可以从 JSON 请求体设置", got.LastResetAt)
	}
}

// 走真实路由的端到端：浏览器提交什么、库里就该是什么。
//
// 单元测试是直接调 service 的，这一条把 gin 绑定、controller、service 校验
// 串起来跑一遍——前端表单少提交一个字段、或字段名对不上，只有这里能发现。
//
// 只走 update 不走 add：addInbound 要从 session 里取登录用户填 UserId，
// 而本包的路由辅助不走登录流程；重置模式的校验与顶时刻逻辑全在编辑这条
// 路径上，覆盖它就够了。
func TestInboundUpdateEndpointRoundTripsTrafficResetMode(t *testing.T) {
	r := newRenewRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	form := func(mode int, expiry int64) string {
		return "remark=%E7%94%B2&protocol=vmess&port=10011&enable=true" +
			"&settings=%7B%7D&streamSettings=%7B%7D&sniffing=%7B%7D" +
			"&trafficResetMode=" + itoa(mode) +
			"&expiryTime=" + itoa64(expiry)
	}
	path := "/aui/inbound/update/" + itoa(in.Id)

	// 1. 选「每月 1 号」——应当成功，并把 LastResetAt 顶到当前
	before := time.Now().Unix() * 1000
	if msg := postForm(t, r, path, form(model.TrafficResetMonthly, 0)); !msg.Success {
		t.Fatalf("保存「每月 1 号」失败: %q", msg.Msg)
	}
	got := reload(t, in.Id)
	if got.TrafficResetMode != model.TrafficResetMonthly {
		t.Fatalf("TrafficResetMode = %d, want %d", got.TrafficResetMode, model.TrafficResetMonthly)
	}
	if got.LastResetAt < before {
		t.Errorf("开启时没有顶 LastResetAt: %d < %d", got.LastResetAt, before)
	}

	// 2. 改成「按订阅周期」但不给到期时间 —— 必须被拒，且库里的值不变
	if msg := postForm(t, r, path, form(model.TrafficResetBillCycle, 0)); msg.Success {
		t.Error("「按订阅周期」+ 无到期时间竟然保存成功了")
	}
	if after := reload(t, in.Id); after.TrafficResetMode != model.TrafficResetMonthly {
		t.Errorf("被拒的保存却改动了库里的值: %d", after.TrafficResetMode)
	}

	// 3. 给上到期时间再存 —— 应当成功
	expiry := time.Now().Add(365 * 24 * time.Hour).UnixMilli()
	if msg := postForm(t, r, path, form(model.TrafficResetBillCycle, expiry)); !msg.Success {
		t.Fatalf("「按订阅周期」+ 到期时间 保存失败: %q", msg.Msg)
	}
	if after := reload(t, in.Id); after.TrafficResetMode != model.TrafficResetBillCycle {
		t.Errorf("TrafficResetMode = %d, want %d", after.TrafficResetMode, model.TrafficResetBillCycle)
	}

	// 4. 未知模式 —— 必须被拒
	if msg := postForm(t, r, path, form(99, expiry)); msg.Success {
		t.Error("未知的重置模式竟然保存成功了")
	}
}

func reload(t *testing.T, id int) *model.Inbound {
	t.Helper()
	got := &model.Inbound{}
	if err := database.GetDB().First(got, id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	return got
}
