package service

import (
	"encoding/json"

	"a-ui/xray"
)

// injectOutboundStats 在生成期打开出站流量统计。
//
// app/proxyman/outbound/handler.go:34-56 的 getStatCounter 在
// policy.ForSystem().Stats.OutboundUplink 为假时**根本不注册计数器**，
// 所以不打开这两个开关，计量出站一个字节都数不到。
//
// 不能只改 web/service/config.json：那是 xrayTemplateConfig 的默认值，
// 管理员一旦在设置页保存过配置，模板就已落库，改 embed 文件对他静默无效。
// 这与 injectAccessLog 是同一个理由、同一种做法。
//
// **只设置这两个 key**，管理员自己配的 levels / handshake / statsUserUplink
// 等一律原样保留——整段替换会在管理员不知情时改掉他的策略配置。
//
// 无条件注入，不看计量池是否为空：policy 在 xray/hot_diff.go:55 的 static
// 名单里，改它必然触发一次整进程重启；无条件注入让这次重启落在升级后的第一次
// 配置变更，时间点可预期、可以写进发版说明。改成「池非空才注入」的话，重启会
// 推迟到上线约一小时后池第一次形成时，变成一次谁都没预料到的全员断线。
//
// 用 map 中转再 Marshal：encoding/json 对 map key 排序，生成逐字节确定。
// policy 写坏时返回错误让整份配置生成失败——xray 会保持旧配置继续跑，是安全
// 的一侧，而错误会出现在面板日志里。
func injectOutboundStats(cfg *xray.Config) error {
	policy := map[string]json.RawMessage{}
	if len(cfg.Policy) > 0 {
		if err := json.Unmarshal(cfg.Policy, &policy); err != nil {
			return err
		}
	}
	system := map[string]json.RawMessage{}
	if raw, ok := policy["system"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &system); err != nil {
			return err
		}
	}
	system["statsOutboundUplink"] = json.RawMessage("true")
	system["statsOutboundDownlink"] = json.RawMessage("true")

	encodedSystem, err := json.Marshal(system)
	if err != nil {
		return err
	}
	policy["system"] = encodedSystem
	encoded, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	cfg.Policy = encoded
	return nil
}
