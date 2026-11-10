package service

import (
	"encoding/json"
	"strings"

	"a-ui/database/model"
)

// inboundRequiresAuth 报告入站是否要求客户端认证。不需要认证的入站（socks noauth、http 没有账号、
// dokodemo-door、shadowsocks 的 none / plain）谁都能连：扫描器的连接在访问日志里同样是「已接受」，
// 自动出差放行会把它们当成被拦的用户，给扫描器所在的省发通行证——等于在那个省开一个公开代理。
//
// settings 解不开时按「需要认证」处理（放行这一侧的选择）：解不开的 settings 本身就让 xray 校验不过、
// 入站起不来，这里判成哪边都放不进人；按「需要认证」处理只是不让一行坏数据把自动出差整个关掉。字段形态与 web/assets/js/model/xray.js 的各协议 Settings 一致。
func inboundRequiresAuth(in *model.Inbound) bool {
	var s struct {
		Auth     string            `json:"auth"`
		Accounts []json.RawMessage `json:"accounts"`
		Method   string            `json:"method"`
	}
	if err := json.Unmarshal([]byte(in.Settings), &s); err != nil {
		return true
	}
	switch strings.ToLower(string(in.Protocol)) {
	case "dokodemo-door":
		return false
	case "socks":
		return s.Auth == "password" && len(s.Accounts) > 0
	case "http":
		return len(s.Accounts) > 0
	case "shadowsocks":
		m := strings.ToLower(s.Method)
		return m != "none" && m != "plain"
	}
	return true
}
