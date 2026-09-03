package service

import (
	"strconv"
	"strings"
	"testing"

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
