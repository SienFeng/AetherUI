package xray

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"

	"a-ui/logger"
)

// 模板里 Config 不认识的顶层键。
//
// 面板把模板反序列化成 Config 再拼入站、注入规则，Config 只有固定的那几个字段，其余顶层键
// （observatory、burstObservatory、metrics……）在这一步被静默丢掉，从来到不了核心：依赖它们的功能
// （比如 leastPing 负载均衡要 observatory）不生效，run -test 照样 Configuration OK。
//
// 只告警、不保留：保留意味着把这些段原样下发，那么升级前被静默忽略了很久的段会在升级那一刻第一次
// 被核心执行——其中任何一个写坏了，整份配置加载失败、整机断网；而且每个新段都要同步进
// Config.Equals 与 ComputeHotDiff 的 static 名单，漏一个就是「改了不生效」。告警让管理员知道它们
// 没有生效，行为零变化。

// knownConfigKeys 是 Config 认识的顶层键，取自它自己的 json tag：给 Config 加字段时不会漂移。
var knownConfigKeys = func() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}()

// DroppedTopLevelKeys 返回 raw 里 Config 不认识、反序列化时会被丢掉的顶层键（升序）。
// raw 不是 JSON 对象时返回 nil。
func DroppedTopLevelKeys(raw []byte) []string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return nil
	}
	dropped := make([]string, 0)
	for key := range top {
		if !knownConfigKeys[key] {
			dropped = append(dropped, key)
		}
	}
	sort.Strings(dropped)
	return dropped
}

var (
	droppedKeysMu  sync.Mutex
	droppedKeysSig string
)

// WarnDroppedTemplateKeys 在模板里有会被丢掉的顶层键时记一条 Warning，返回这次是否记了。
// 模板在每次生成配置时都会被读（10 秒的消费任务、页面轮询、保存时校验），同一组键只记一次。
func WarnDroppedTemplateKeys(raw []byte) bool {
	dropped := DroppedTopLevelKeys(raw)
	sig := strings.Join(dropped, ",")
	droppedKeysMu.Lock()
	changed := sig != droppedKeysSig
	droppedKeysSig = sig
	droppedKeysMu.Unlock()
	if !changed || sig == "" {
		return false
	}
	logger.Warning("xray 模板里的顶层配置段 [", sig, "] 面板不认识，生成配置时会被丢弃、不会下发给 xray；"+
		"依赖它们的功能（例如 leastPing 负载均衡依赖的 observatory）不会生效")
	return true
}
