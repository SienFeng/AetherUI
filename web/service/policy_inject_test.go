package service

import (
	"encoding/json"
	"testing"

	"a-ui/xray"
)

func decodePolicySystem(t *testing.T, cfg *xray.Config) map[string]any {
	t.Helper()
	var policy struct {
		System map[string]any `json:"system"`
	}
	if len(cfg.Policy) == 0 {
		return nil
	}
	if err := json.Unmarshal(cfg.Policy, &policy); err != nil {
		t.Fatalf("解析 policy: %v", err)
	}
	return policy.System
}

func TestInjectOutboundStatsAddsBothKeys(t *testing.T) {
	cfg := &xray.Config{}
	if err := injectOutboundStats(cfg); err != nil {
		t.Fatalf("injectOutboundStats: %v", err)
	}
	system := decodePolicySystem(t, cfg)
	if system["statsOutboundUplink"] != true || system["statsOutboundDownlink"] != true {
		t.Errorf("policy.system = %+v，期望两个出站统计开关都为 true——"+
			"getStatCounter 在它们为假时根本不注册计数器，计量出站一个字节都数不到", system)
	}
}

func TestInjectOutboundStatsKeepsAdminKeys(t *testing.T) {
	cfg := &xray.Config{Policy: []byte(
		`{"levels":{"0":{"handshake":8}},"system":{"statsInboundUplink":true,"statsUserUplink":true}}`)}
	if err := injectOutboundStats(cfg); err != nil {
		t.Fatalf("injectOutboundStats: %v", err)
	}
	system := decodePolicySystem(t, cfg)
	// 管理员自己配的 key 一个都不能丢：这里只设两个 key，不是整段替换。
	for _, k := range []string{"statsInboundUplink", "statsUserUplink"} {
		if system[k] != true {
			t.Errorf("policy.system[%q] 丢失，期望原样保留", k)
		}
	}
	var policy map[string]any
	if err := json.Unmarshal(cfg.Policy, &policy); err != nil {
		t.Fatalf("解析 policy: %v", err)
	}
	if _, ok := policy["levels"]; !ok {
		t.Error("policy.levels 丢失，期望原样保留")
	}
}

func TestInjectOutboundStatsIsByteStable(t *testing.T) {
	// 生成逐字节确定：Config.Equals 按字节比 Policy，抖动会让那个 10 秒的
	// cron 判定配置一直在变，不停重启 xray。
	first := &xray.Config{Policy: []byte(`{"system":{"statsInboundUplink":true}}`)}
	if err := injectOutboundStats(first); err != nil {
		t.Fatalf("首次: %v", err)
	}
	second := &xray.Config{Policy: []byte(`{"system":{"statsInboundUplink":true}}`)}
	if err := injectOutboundStats(second); err != nil {
		t.Fatalf("再次: %v", err)
	}
	if string(first.Policy) != string(second.Policy) {
		t.Errorf("两次结果不同：\n%s\n%s", first.Policy, second.Policy)
	}
	// 幂等：对已经注入过的配置再注入一次，结果必须完全一样。
	again := &xray.Config{Policy: first.Policy}
	if err := injectOutboundStats(again); err != nil {
		t.Fatalf("幂等: %v", err)
	}
	if string(again.Policy) != string(first.Policy) {
		t.Errorf("重复注入改变了结果：\n%s\n%s", first.Policy, again.Policy)
	}
}

func TestInjectOutboundStatsRejectsBrokenPolicy(t *testing.T) {
	// 管理员把 policy 写坏时让整份配置生成失败：xray 会保持旧配置继续跑，
	// 是安全的一侧，而错误会出现在面板日志里。
	cfg := &xray.Config{Policy: []byte(`{"system":123}`)}
	if err := injectOutboundStats(cfg); err == nil {
		t.Error("期望报错——policy.system 不是对象时不能静默吞掉")
	}
}
