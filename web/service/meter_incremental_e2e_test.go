package service

import (
	"context"
	"strings"
	"testing"
	"time"

	routerService "github.com/xtls/xray-core/app/router/command"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// coreRuleList 返回运行中核心的规则表（出站|ruleTag），以及面板记下的那份配置里的规则表，二者应逐条相同。
func coreRuleList(t *testing.T) (got, want []string) {
	t.Helper()
	lock.Lock()
	cfg, port := p.GetConfig(), p.GetAPIPort()
	lock.Unlock()
	api := &xray.XrayAPI{}
	if err := api.Init(port); err != nil {
		t.Fatalf("连接控制面: %v", err)
	}
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := (*api.RoutingServiceClient).ListRule(ctx, &routerService.ListRuleRequest{})
	if err != nil {
		t.Fatalf("ListRule: %v", err)
	}
	for _, r := range resp.Rules {
		got = append(got, r.Tag+"|"+r.RuleTag)
	}
	for _, r := range decodeRules(t, cfg) {
		tag, _ := r["outboundTag"].(string)
		ruleTag, _ := r["ruleTag"].(string)
		want = append(want, tag+"|"+ruleTag)
	}
	return got, want
}

// 换池在真 xray 上走增量下发：进程不重启，核心的规则表与整表下发的结果逐条相同，新进池的域名确实落到它的计量出站。
// 两种形态都要过：模板默认（不解析域名，计量规则不带守卫）与生产机的 IPIfNonMatch（计量域名规则带 IP 守卫）。
func TestMeterPoolChangeIsAppliedIncrementallyAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	requireXrayRoutingService(t)
	requirePgrep(t)
	for _, resolve := range []bool{false, true} {
		name := "AsIs"
		if resolve {
			name = "IPIfNonMatch"
		}
		t.Run(name, func(t *testing.T) {
			requireXrayAPIPortFree(t)
			setupMeterPoolTest(t)
			useTemplateWithAPIPort(t, hotReloadAPIPort, nil)
			if resolve {
				if err := (&SettingService{}).setInt("ipRuleResolveDomain", 1); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				lock.Lock()
				defer lock.Unlock()
				if p != nil {
					_ = p.Stop()
					p = nil
				}
				result = ""
			})

			socksPort := freePort(t)
			in := &model.Inbound{
				UserId: 1, Port: socksPort, Protocol: model.Protocol("socks"), Enable: true,
				Tag: "inbound-meter-e2e", Listen: "127.0.0.1",
				Settings: `{"auth":"noauth","udp":false}`, StreamSettings: `{"network":"tcp","security":"none"}`, Sniffing: "{}",
			}
			if err := database.GetDB().Save(in).Error; err != nil {
				t.Fatalf("save inbound: %v", err)
			}
			putPoolRowAt(t, in.Id, "zeta.com", 0, 1000)
			putPoolRowAt(t, in.Id, "mid.com", 0, 1000)
			putPoolRowAt(t, in.Id, "192.0.2.10", 0, 1000)

			xs := &XrayService{}
			if err := xs.RestartXray(true); err != nil {
				t.Fatalf("RestartXray(true): %v", err)
			}
			waitForPort(t, socksPort)
			pid := xrayChildPID(t)
			if got, want := coreRuleList(t); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("启动后核心规则表与配置不同：\n%v\n%v", got, want)
			}

			// 换池：字母序排最前的新域名进池，一个旧域名退池。
			putPoolRowAt(t, in.Id, "alpha.com", 0, 5000)
			if err := database.GetTrafficDB().Model(&model.MeterDomain{}).
				Where("inbound_id = ? and domain = ?", in.Id, "mid.com").
				Update("cooldown_until", time.Now().Add(24*time.Hour).Unix()).Error; err != nil {
				t.Fatal(err)
			}
			logs := captureLogs(t)
			err := xs.RestartXray(false)
			out := logs()
			if err != nil {
				t.Fatalf("RestartXray(false): %v", err)
			}
			// 删：退池的 mid.com，与排在新域名后面的 IP 规则；加：alpha.com 与那条 IP 规则。
			if !strings.Contains(out, "xray 计量池改动已增量下发：删除 2 条、追加 2 条计量规则") {
				t.Fatalf("换池没有走增量下发，日志：\n%s", out)
			}
			if got := xrayChildPID(t); got != pid {
				t.Fatalf("换池重启了 xray：PID %d → %d", pid, got)
			}
			got, want := coreRuleList(t)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("增量下发后核心规则表与配置不同：\n%v\n%v", got, want)
			}
			if !strings.Contains(strings.Join(got, ","), model.MeterTag(in.Id, "alpha.com")) ||
				strings.Contains(strings.Join(got, ","), model.MeterTag(in.Id, "mid.com")) {
				t.Fatalf("核心规则表没有反映换池：%v", got)
			}

			if resolve {
				return // 带守卫的规则要目标先解析出 IP，TestRoute 只给域名时第一遍不会命中，这里只核对规则表
			}
			api := &xray.XrayAPI{}
			if err := api.Init(p.GetAPIPort()); err != nil {
				t.Fatal(err)
			}
			defer api.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if tag, _, err := api.TestRoute(ctx, in.Tag, "www.alpha.com", nil); err != nil || tag != model.MeterTag(in.Id, "alpha.com") {
				t.Fatalf("新进池的域名应走它的计量出站：tag=%q err=%v", tag, err)
			}
			if tag, _, err := api.TestRoute(ctx, in.Tag, "www.mid.com", nil); err != nil || model.IsMeterTag(tag) {
				t.Fatalf("退池的域名不该再走计量出站：tag=%q err=%v", tag, err)
			}
		})
	}
}
