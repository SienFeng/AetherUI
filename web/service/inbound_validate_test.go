package service

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// 这份 streamSettings 复刻了一次真实事故：管理员在入站表单里开了 TLS，
// 却没填证书路径。xray 加载配置是全有或全无的，这一个入站会让**整份配置**
// 加载失败，机器上所有用户一起断网，而面板首页只显示一个 error。
const tlsWithoutCertificate = `{"network":"ws","security":"tls",` +
	`"tlsSettings":{"serverName":"","certificates":[{"certificateFile":"","keyFile":""}]},` +
	`"wsSettings":{"path":"/","headers":{}}}`

const plainTCPStream = `{"network":"tcp","security":"none","tcpSettings":{"header":{"type":"none"}}}`

func vlessSettings() string {
	return `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811","flow":""}],"decryption":"none","fallbacks":[]}`
}

func newInboundFor(port int, stream string, enable bool) *model.Inbound {
	return &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS,
		Tag:    "inbound-" + strconv.Itoa(port),
		Enable: enable, Settings: vlessSettings(), StreamSettings: stream, Sniffing: "{}",
	}
}

func TestAddInboundAcceptsValidConfig(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	if err := (&InboundService{}).AddInbound(newInboundFor(40101, plainTCPStream, true)); err != nil {
		t.Fatalf("合法入站应当能保存: %v", err)
	}
}

func TestAddInboundRejectsTLSWithoutCertificate(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	err := (&InboundService{}).AddInbound(newInboundFor(40102, tlsWithoutCertificate, true))
	if err == nil {
		t.Fatal("开了 TLS 却没有证书的入站必须被拒绝——放它进库会让整份 xray 配置加载失败，所有人一起断网")
	}
	if !strings.Contains(err.Error(), "xray") {
		t.Errorf("错误信息 %q 应当指出是 xray 校验没过", err)
	}
	// 拒绝必须发生在落库之前，否则脏数据仍然留在库里，下次重启照样炸。
	var count int64
	if err := database.GetDB().Model(model.Inbound{}).Where("port = ?", 40102).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Error("校验失败的入站被写进了数据库")
	}
}

func TestUpdateInboundRejectsTLSWithoutCertificate(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	s := InboundService{}
	in := newInboundFor(40103, plainTCPStream, true)
	if err := s.AddInbound(in); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}

	in.StreamSettings = tlsWithoutCertificate
	if err := s.UpdateInbound(in); err == nil {
		t.Fatal("改成「TLS 无证书」必须被拒绝")
	}
	// 库里必须还是改动前的样子。
	got, err := s.GetInbound(in.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.StreamSettings != plainTCPStream {
		t.Error("校验失败却改动了库里的数据")
	}
}

// 编辑一个已存在的入站时，完整配置里那份旧的必须先摘掉，
// 否则候选对象会和它自己撞端口 / 撞 tag 而被误拒。
func TestUpdateInboundDoesNotCollideWithItsOwnOldCopy(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	s := InboundService{}
	in := newInboundFor(40104, plainTCPStream, true)
	if err := s.AddInbound(in); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}

	in.Remark = "改个备注"
	if err := s.UpdateInbound(in); err != nil {
		t.Fatalf("只改备注却被拒: %v", err)
	}
}

// 改端口时旧入站的 tag 也会跟着变，摘除必须按**旧** tag 来。
func TestUpdateInboundHandlesPortChange(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	s := InboundService{}
	in := newInboundFor(40105, plainTCPStream, true)
	if err := s.AddInbound(in); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}

	in.Port = 40106
	if err := s.UpdateInbound(in); err != nil {
		t.Fatalf("改端口被拒: %v", err)
	}
	got, err := s.GetInbound(in.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 40106 || got.Tag != "inbound-40106" {
		t.Errorf("端口/tag = %d/%s，期望 40106/inbound-40106", got.Port, got.Tag)
	}
}

// 停用的入站不进 xray 配置，但仍然要校验：等管理员某天启用它才发现配错了，
// 那时炸的是全部用户。
func TestAddInboundValidatesEvenWhenDisabled(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	if err := (&InboundService{}).AddInbound(newInboundFor(40107, tlsWithoutCertificate, false)); err == nil {
		t.Fatal("停用的入站也要校验：现在放过去，等它被启用时炸的是所有人")
	}
}

// 停用着的坏入站（升级前存下的、核心升级后不再认的配置——比如 26.x 拒绝的 mKCP header）被重新启用时，
// 必须先过同一道真实 xray 校验：列表开关、重置流量拉回、续期拉回都不经过编辑表单，放过去的话核心按一份
// 它拒绝的配置整进程重启，所有用户一起断网而面板首页仍显示 running。
func TestReviveRejectsInboundThatCoreRefuses(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	bad := func(port int, byTraffic bool) *model.Inbound {
		in := newInboundFor(port, tlsWithoutCertificate, false)
		in.DisabledByTraffic = byTraffic
		// 直接落库，模拟升级前存下、当时校验不出问题的入站。
		if err := database.GetDB().Create(in).Error; err != nil {
			t.Fatal(err)
		}
		return in
	}
	stillDisabled := func(id int) bool {
		var in model.Inbound
		if err := database.GetDB().First(&in, id).Error; err != nil {
			t.Fatal(err)
		}
		return !in.Enable
	}
	s := &InboundService{}

	a := bad(40121, false)
	if err := s.SetInboundEnable(a.Id, true); err == nil {
		t.Error("列表开关启用一个核心不认的入站，应当被拒绝")
	}
	if !stillDisabled(a.Id) {
		t.Error("校验没过的入站被启用了")
	}
	if err := s.SetInboundEnable(a.Id, false); err != nil {
		t.Errorf("停用不需要校验: %v", err)
	}

	b := bad(40122, true)
	if _, err := s.ResetInboundTraffic(b.Id, time.Now()); err == nil {
		t.Error("重置流量会把超额停用的入站拉回来，核心不认它时应当被拒绝")
	}
	if !stillDisabled(b.Id) {
		t.Error("重置流量把一个核心不认的入站拉回来了")
	}

	c := bad(40123, false)
	if err := s.RenewInbound(c.Id, 30, 0); err == nil {
		t.Error("续期会重新启用入站，核心不认它时应当被拒绝")
	}
	if !stillDisabled(c.Id) {
		t.Error("续期把一个核心不认的入站启用了")
	}
}

// 周期重置是无人值守的后台任务：核心不认的超额停用入站照样清零，但不拉回来（拉回来就是全员断网），
// 标记保留——管理员修好之后下一次启用或续期照常。
func TestResetDueTrafficDoesNotReviveInboundThatCoreRefuses(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	in := newInboundFor(40124, tlsWithoutCertificate, false)
	in.DisabledByTraffic = true
	in.TrafficResetMode = model.TrafficResetMonthly
	in.Up, in.Down = 100, 200
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatal(err)
	}
	reset, revived, err := (&InboundService{}).ResetDueTraffic(time.Now(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var got model.Inbound
	if err := database.GetDB().First(&got, in.Id).Error; err != nil {
		t.Fatal(err)
	}
	if reset != 1 || got.Up != 0 || got.Down != 0 {
		t.Errorf("周期到了应当照常清零: reset=%d up=%d down=%d", reset, got.Up, got.Down)
	}
	if revived != 0 || got.Enable || !got.DisabledByTraffic {
		t.Errorf("核心不认的入站不该被拉回来，标记也要保留: revived=%d enable=%v byTraffic=%v", revived, got.Enable, got.DisabledByTraffic)
	}
}
