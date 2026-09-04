package bootstrap

import (
	"encoding/json"
	"fmt"

	"a-ui/database/model"
)

// RealityParams 的密钥、UUID、shortId 由调用方生成后传入，不在这里生成：
// 内部生成会让输出不确定，无法用 golden 文件锁住「与前端模型一致」这件事。
type RealityParams struct {
	Port       int
	UUID       string
	PrivateKey string
	PublicKey  string
	ShortID    string
	Target     string // 形如 www.tesla.com:443
	ServerName string
	Remark     string
}

// BuildRealityInbound 组装一个 VLESS + Vision + REALITY 入站。
//
// 字段名以 web/assets/js/model/xray.js 的模型为准，不是以 xray-core 的
// 配置文档为准——两者大部分重合，但本项目的模型有自己的约定：
// REALITY 的伪装目标叫 target（xray.js:558），serverNames/shortIds 在
// 数据库里是数组而在表单模型里是逗号分隔串（xray.js:544）。
// 字段名与字段的增删由 bootstrap/testdata/gen_golden.js 生成的 golden 文件
// 核对，不是照 xray-core 文档手写——tcpSettings 只有 header 一个字段，没有
// acceptProxyProtocol（TcpStreamSettings.toJson，xray.js:185-193）。
// 注意 golden 断言的**不包括字段顺序**：测试两侧都先解成 map[string]any
// 再重新 Marshal，而 encoding/json 对 map key 排序，顺序差异在比较之前就
// 被抹平了。要锁顺序得改成逐字节比较原始字符串，那会把无意义的格式差异
// 也一起锁死，代价大于收益。
func BuildRealityInbound(p RealityParams) (*model.Inbound, error) {
	settings := map[string]any{
		"clients": []map[string]any{{
			"id":   p.UUID,
			"flow": "xtls-rprx-vision",
		}},
		"decryption": "none",
		"fallbacks":  []any{},
	}
	stream := map[string]any{
		"network":  "tcp",
		"security": "reality",
		"realitySettings": map[string]any{
			"show":         false,
			"xver":         0,
			"target":       p.Target,
			"serverNames":  []string{p.ServerName},
			"privateKey":   p.PrivateKey,
			"mldsa65Seed":  "",
			"minClientVer": "",
			"maxClientVer": "",
			"maxTimeDiff":  0,
			"shortIds":     []string{p.ShortID},
			"settings": map[string]any{
				"publicKey":     p.PublicKey,
				"fingerprint":   "chrome",
				"serverName":    "",
				"spiderX":       "/",
				"mldsa65Verify": "",
			},
		},
		"tcpSettings": map[string]any{
			"header": map[string]any{"type": "none"},
		},
	}
	sniffing := map[string]any{
		"enabled":      true,
		"destOverride": []string{"http", "tls", "quic"},
	}

	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("序列化 settings: %w", err)
	}
	streamJSON, err := json.Marshal(stream)
	if err != nil {
		return nil, fmt.Errorf("序列化 streamSettings: %w", err)
	}
	sniffingJSON, err := json.Marshal(sniffing)
	if err != nil {
		return nil, fmt.Errorf("序列化 sniffing: %w", err)
	}

	return &model.Inbound{
		Enable:         true,
		Remark:         p.Remark,
		Listen:         "",
		Port:           p.Port,
		Protocol:       model.VLESS,
		Settings:       string(settingsJSON),
		StreamSettings: string(streamJSON),
		Sniffing:       string(sniffingJSON),
		// Tag 由 InboundService.UpdateInbound 按端口生成；新增时这里给出
		// 同样形态的值，与面板里手工新建的入站保持一致。
		Tag: fmt.Sprintf("inbound-%v", p.Port),
	}, nil
}
