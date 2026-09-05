package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/entity"
	"a-ui/xray"
)

// runXrayTest 把一份配置交给真实的 xray 做语法与语义校验。
//
// 采用 fail open：只有 xray 明确判定配置非法时才返回错误。二进制缺失、
// 命令行参数不被老版本识别、超时等情况一律返回 nil 并记日志——校验器
// 自身的故障绝不能变成用户无法保存配置的门禁。
func runXrayTest(cfg map[string]any) error {
	binaryPath := xray.GetBinaryPath()
	if _, err := os.Stat(binaryPath); err != nil {
		logger.Debug("skip config validation, xray binary not found:", err)
		return nil
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "a-ui-validate-*.json")
	if err != nil {
		logger.Warning("skip config validation, cannot create temp file:", err)
		return nil
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		logger.Warning("skip config validation, cannot write temp file:", err)
		return nil
	}
	file.Close()

	cmd := exec.Command(binaryPath, "run", "-test", "-c", file.Name())
	done := make(chan struct{})
	var output []byte
	var runErr error
	go func() {
		output, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		logger.Warning("skip config validation, xray -test timed out")
		return nil
	}

	text := string(output)
	if runErr == nil || strings.Contains(text, "Configuration OK") {
		return nil
	}
	// 老版本 xray 可能不认 "run -test" 这套参数，此时不是配置的问题。
	if strings.Contains(text, "unknown command") || strings.Contains(text, "flag provided but not defined") {
		logger.Warning("skip config validation, xray does not support 'run -test':", firstLine(text))
		return nil
	}
	return common.NewError("xray 校验未通过:", lastMeaningfulLine(text))
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return s
}

// lastMeaningfulLine 取最后一行非空、非版权横幅的输出，那里才是真正的报错。
func lastMeaningfulLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" ||
			strings.HasPrefix(line, "Xray ") ||
			strings.HasPrefix(line, "A unified platform") {
			continue
		}
		return line
	}
	return firstLine(s)
}

// generatedConfigJSON 返回「不做本次改动的话，xray 会拿到的那份配置」，
// 即模板 + 全部启用入站 + 注入的出站与分流规则。
func generatedConfigJSON() ([]byte, error) {
	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}

func decodeConfig(data []byte) (map[string]any, error) {
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateWithFullConfig 校验「应用本次改动之后」的完整配置。
//
// 设计 §5.4.2 要求校验的就是完整配置：只包住单个对象的最小配置在原理上发现
// 不了组合层面的冲突——与注入器发出的 tag 撞名、proxySettings.tag 指向不存在
// 的出站，这些只有放进完整配置才暴露。C1（备注写 block 导致全员断网）正是
// 因为孤立校验看不见它，才一路通过了全部审查。
//
// 沿用 runXrayTest 的 fail open 原则，另加两条边界：
//   - 取不到完整配置（模板损坏、库不可用）时退回最小配置校验，至少还能抓出
//     对象自身的错误，而不是干脆放弃校验。
//   - 改动之前配置就已经不合法时放行：问题不是本次改动引入的。否则一个早已
//     存在的模板错误会把管理员锁在门外，连修复用的操作都做不了。
func validateWithFullConfig(apply func(cfg map[string]any), minimal map[string]any) error {
	data, err := generatedConfigJSON()
	if err != nil {
		logger.Warning("fall back to minimal config validation, cannot build full config:", err)
		return runXrayTest(minimal)
	}
	prospective, err := decodeConfig(data)
	if err != nil {
		logger.Warning("fall back to minimal config validation, cannot decode full config:", err)
		return runXrayTest(minimal)
	}

	apply(prospective)
	testErr := runXrayTest(prospective)
	if testErr == nil {
		return nil
	}

	baseline, err := decodeConfig(data)
	if err == nil && runXrayTest(baseline) != nil {
		logger.Warning("config was already invalid before this change, allowing the save:", testErr)
		return nil
	}
	return testErr
}

// appendOutbound 把候选出站追加到配置末尾。追加而非插入是为了不改变默认出站
// （xray 取 outbounds 的第一个），与注入器的第一条不变量一致。
func appendOutbound(cfg map[string]any, ob map[string]any) {
	outbounds, _ := cfg["outbounds"].([]any)
	cfg["outbounds"] = append(outbounds, ob)
}

// removeOutboundByTag 摘掉完整配置里同 tag 的那份旧出站。编辑一个已存在的节点
// 时必须先摘掉，否则它会和候选对象自己撞名，造成误拒。
func removeOutboundByTag(cfg map[string]any, tag string) {
	// 保留 tag 对应的是注入器自己发出的黑洞出站，不是节点表里的东西，绝不能摘。
	// 摘掉它，一个 tag 为 a-ui-block 的历史脏数据节点就会「校验通过」，
	// 而它在生成端本来就是被跳过的。
	if tag == "" || model.IsReservedTag(tag) {
		return
	}
	outbounds, _ := cfg["outbounds"].([]any)
	for i, item := range outbounds {
		ob, ok := item.(map[string]any)
		if !ok || ob["tag"] != tag {
			continue
		}
		cfg["outbounds"] = append(outbounds[:i:i], outbounds[i+1:]...)
		return
	}
}

func minimalOutboundConfig(ob map[string]any) map[string]any {
	return map[string]any{
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "settings": map[string]any{}},
			ob,
		},
	}
}

// ValidateOutbound 校验一个新增的出站节点。在落库之前调用，因此不需要事务回滚。
func ValidateOutbound(ob map[string]any) error {
	return ValidateOutboundReplacing(ob, "")
}

// ValidateOutboundReplacing 校验一个编辑后的出站节点。replacedTag 是它在库里
// 已有的 tag（tag 不可变），校验时要把完整配置里那份旧的先摘掉。
func ValidateOutboundReplacing(ob map[string]any, replacedTag string) error {
	return validateWithFullConfig(func(cfg map[string]any) {
		removeOutboundByTag(cfg, replacedTag)
		appendOutbound(cfg, ob)
	}, minimalOutboundConfig(ob))
}

// validateRuleCondition 把一条只带单个条件的探针规则追加到完整配置末尾，
// 交给真实 xray 判定。field 是 xray 路由规则里的条件键（"domain" / "ip"）。
//
// 出站指向注入器始终会注入的黑洞，因此这条探针不会引入悬空引用——
// xray 对悬空 outboundTag 不报错，只会静默回落直连，所以这一点是承重的。
func validateRuleCondition(field string, values []string) error {
	probe := map[string]any{
		"type": "field", field: values, "outboundTag": model.BlockOutboundTag,
	}
	minimal := map[string]any{
		"outbounds": []any{
			map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}},
		},
		"routing": map[string]any{
			"rules": []any{
				map[string]any{"type": "field", field: values, "outboundTag": "direct"},
			},
		},
	}
	return validateWithFullConfig(func(cfg map[string]any) {
		routing, _ := cfg["routing"].(map[string]any)
		if routing == nil {
			routing = map[string]any{}
			cfg["routing"] = routing
		}
		rules, _ := routing["rules"].([]any)
		routing["rules"] = append(rules, probe)
	}, minimal)
}

// ValidateDomains 校验域名列表，能抓出不存在的 geosite 类别（checkFile 会打开
// dat 找 code，见 common/geodata/geodat_loader.go:16）与非法正则。
func ValidateDomains(domains []string) error {
	return validateRuleCondition("domain", domains)
}

// ValidateCidrs 校验 IP 段列表，能抓出不存在的 geoip 类别与非法的 CIDR。
//
// 空列表直接放行：ip 为空数组时探针只剩 outboundTag 一个非条件字段，
// xray 会报 "this rule has no effective fields" 而整份配置被判非法——
// 一个「这个组没有 IP 段」的正常状态会变成保存失败。
func ValidateCidrs(cidrs []string) error {
	if len(cidrs) == 0 {
		return nil
	}
	return validateRuleCondition("ip", cidrs)
}

// removeInboundByTag 摘掉完整配置里同 tag 的那份旧入站。
//
// 编辑一个已存在的入站时必须先摘掉：候选对象与库里那份端口相同、tag 相同，
// 不摘的话它会和自己撞名而被误拒。
func removeInboundByTag(cfg map[string]any, tag string) {
	if tag == "" {
		return
	}
	inbounds, _ := cfg["inbounds"].([]any)
	for i, item := range inbounds {
		ib, ok := item.(map[string]any)
		if !ok || ib["tag"] != tag {
			continue
		}
		cfg["inbounds"] = append(inbounds[:i:i], inbounds[i+1:]...)
		return
	}
}

func appendInbound(cfg map[string]any, ib map[string]any) {
	inbounds, _ := cfg["inbounds"].([]any)
	cfg["inbounds"] = append(inbounds, ib)
}

func minimalInboundConfig(ib map[string]any) map[string]any {
	return map[string]any{
		"inbounds": []any{ib},
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "settings": map[string]any{}},
		},
	}
}

// ValidateInboundReplacing 校验一个入站在应用到完整配置之后是否仍然合法。
//
// 存在的理由是一次真实事故：管理员在表单里开了 TLS 却没填证书路径，保存时
// 一切正常，直到某次重启 xray 才发现——**xray 加载配置是全有或全无的**，
// 这一个入站让整份配置加载失败，机器上所有用户一起断网，而面板首页只显示
// 一个 error，看不出是哪个入站的问题。把校验放在保存时，问题就停在制造它的
// 那一刻。
//
// replacedTag 是这个入站在库里已有的 tag（改端口时 tag 会变，必须传旧的）。
// 沿用 validateWithFullConfig 的 fail open 边界：xray 自身故障、取不到完整
// 配置、改动之前配置就已经不合法，三种情况一律放行。
func ValidateInboundReplacing(ib map[string]any, replacedTag string) error {
	return validateWithFullConfig(func(cfg map[string]any) {
		removeInboundByTag(cfg, replacedTag)
		appendInbound(cfg, ib)
	}, minimalInboundConfig(ib))
}

// ValidateSettings 校验「按这份设置保存下去之后」的完整配置，真实 xray 说了算。
//
// 设计 §8.6 要求这一步，它不是装饰。一个实例：dnsServers 写成 1.1.1.1:53
// 语法上无懈可击，xray 却直接拒绝启动（exit 23）；而 dns 在 xray/hot_diff.go
// 的 static 名单里，保存必然触发整进程重启，Process.Start() 又把 cmd.Run()
// 丢进 goroutine、从不回传启动失败——没有这一步，管理员看到的是「保存成功」、
// 面板首页 running、errorMsg 为空，而机器上所有用户已经断网。有了这一步，
// 同一个错误在保存那一刻就被顶回来，还带着 xray 自己的报错原文。
//
// 只对真正会进入生成配置的设置项做这一步：dnsServers 与 ipRuleResolveDomain。
// 端口、时区、保留天数这些压根不进 xray 配置，为它们 exec 一次真实 xray 只是
// 给每一次保存平白加上一秒延迟和一个临时文件。
//
// xrayTemplateConfig 刻意不在此列。换模板意味着整条生成链（模板 + 全部启用
// 入站 + RoutingInjector + DNSInjector）都要按新模板重跑一遍，而 GetXrayConfig
// 是从库里读设置的；要让它按候选值跑，就得再造一条与它平行的生成链。那条链
// 一旦与 GetXrayConfig 漂移，「校验通过的配置」与「真正下发的配置」就不是同一
// 份——比不校验更危险，因为它会给出一个假的安全感。模板的语法错误仍由
// entity.CheckValid 的 json.Unmarshal 挡住。
//
// 沿用 validateWithFullConfig 的三条 fail open 边界，一条都不收紧。
func ValidateSettings(candidate *entity.AllSetting) error {
	if !settingsAffectXrayConfig(candidate) {
		return nil
	}
	return validateWithFullConfig(func(cfg map[string]any) {
		applyCandidateDNS(cfg, candidate)
		applyCandidateDomainStrategy(cfg, candidate)
	}, minimalSettingsConfig(candidate))
}

// minimalSettingsConfig 是取不到完整配置时的退路：只带候选 dns 段和一个
// freedom 出站。它发现不了组合层面的冲突，但至少还能抓出 dns 段自身的错误，
// 而不是干脆放弃校验。绝不能传 nil——那会被 json.Marshal 成 "null"，xray
// 必然拒绝，一条 fail open 的退路就变成了无条件拒绝保存。
func minimalSettingsConfig(candidate *entity.AllSetting) map[string]any {
	cfg := map[string]any{
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "settings": map[string]any{}},
		},
	}
	if servers := buildDNSServers(candidate.DNSServers); len(servers) > 0 {
		cfg["dns"] = map[string]any{"servers": servers}
	}
	return cfg
}

// settingsAffectXrayConfig 报告这次保存是否动了会进入 xray 配置的设置项。
//
// 读旧值本身出错时返回 true（照常校验）：那说明库有问题，而漏掉一次校验的
// 后果远比多跑一次 xray 严重。
func settingsAffectXrayConfig(candidate *entity.AllSetting) bool {
	settingService := SettingService{}
	oldServers, err := settingService.GetDNSServers()
	if err != nil {
		return true
	}
	oldResolve, err := settingService.GetIPRuleResolveDomain()
	if err != nil {
		return true
	}
	return candidate.DNSServers != oldServers ||
		(candidate.IPRuleResolveDomain == 1) != oldResolve
}

// templateSection 取候选模板里某个顶层键的原值。第二个返回值为 false 表示
// 模板里根本没有这个键（与「值为 null」要分开）。
func templateSection(templateConfig, key string) (any, bool) {
	var tmpl map[string]any
	if err := json.Unmarshal([]byte(templateConfig), &tmpl); err != nil {
		// entity.CheckValid 已经确认它能反序列化成 xray.Config，走到这里
		// 说明是别的形状问题。保持配置原样即可，不能因此把保存卡住。
		return nil, false
	}
	v, ok := tmpl[key]
	return v, ok
}

// applyCandidateDNS 把 DNSInjector 在这份候选设置下会写出的 dns 段应用到
// 完整配置上。
//
// 必须自己应用，不能指望 GetXrayConfig：它读的是库里的旧值，基线里那段 dns
// 是旧 dnsServers 注入的结果。尤其是「把 dnsServers 清空」这一次保存——不换
// 回模板里的原文，校验的就是一份根本不会下发的配置。
//
// 只处理 dns 段，不动 freedom 出站 settings 里的 domainStrategy：那一项
// （UseIP）恒定合法，加不加都不改变 xray 接不接受这份配置，而要正确地加/删
// 它得把 DNSInjector 的整套出站遍历再走一遍。
func applyCandidateDNS(cfg map[string]any, candidate *entity.AllSetting) {
	servers := buildDNSServers(candidate.DNSServers)
	if len(servers) > 0 {
		cfg["dns"] = map[string]any{"servers": servers}
		return
	}
	if v, ok := templateSection(candidate.XrayTemplateConfig, "dns"); ok {
		cfg["dns"] = v
	} else {
		delete(cfg, "dns")
	}
}

// applyCandidateDomainStrategy 镜像 RoutingInjector 对 routing.domainStrategy
// 的处理：开关为 1 时写 IPIfNonMatch，为 0 时【不碰】——模板里的原值才是最终值。
func applyCandidateDomainStrategy(cfg map[string]any, candidate *entity.AllSetting) {
	routing, _ := cfg["routing"].(map[string]any)
	if candidate.IPRuleResolveDomain == 1 {
		if routing == nil {
			routing = map[string]any{}
			cfg["routing"] = routing
		}
		routing["domainStrategy"] = "IPIfNonMatch"
		return
	}
	if routing == nil {
		// 没有 routing 段就没有 domainStrategy 可言，凭空造一个空对象
		// 只会让校验的那份配置与真正生成的那份多出一处差异。
		return
	}
	tmplRouting, _ := templateSection(candidate.XrayTemplateConfig, "routing")
	if m, ok := tmplRouting.(map[string]any); ok {
		if v, ok := m["domainStrategy"]; ok {
			routing["domainStrategy"] = v
			return
		}
	}
	delete(routing, "domainStrategy")
}
