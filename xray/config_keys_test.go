package xray

import (
	"reflect"
	"testing"
)

// 模板里 Config 不认识的顶层键（observatory、burstObservatory、metrics……）在反序列化时被静默丢掉，
// 从来到不了核心：依赖它们的功能（leastPing 负载均衡要 observatory）不生效，面板一个字都不说。
func TestDroppedTopLevelKeys(t *testing.T) {
	raw := []byte(`{"log":{},"inbounds":[],"observatory":{"subjectSelector":["a-ui-"]},
		"burstObservatory":{},"metrics":{"tag":"metrics"},"fakeDns":[],"reverse":{}}`)
	got := DroppedTopLevelKeys(raw)
	want := []string{"burstObservatory", "metrics", "observatory"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DroppedTopLevelKeys = %v，期望 %v", got, want)
	}
	if got := DroppedTopLevelKeys([]byte(`{"log":{},"routing":{}}`)); len(got) != 0 {
		t.Fatalf("全是认识的键，却报了 %v", got)
	}
	if got := DroppedTopLevelKeys([]byte(`not json`)); got != nil {
		t.Fatalf("不是 JSON 对象时应当返回 nil，实际 %v", got)
	}
}

// 每次生成配置都会读模板（10 秒的消费任务、页面轮询、保存时校验），同一组键只记一次；
// 键变了再记；修好之后再出现照样记。
func TestWarnDroppedTemplateKeysOncePerSignature(t *testing.T) {
	droppedKeysMu.Lock()
	droppedKeysSig = ""
	droppedKeysMu.Unlock()

	withObs := []byte(`{"observatory":{}}`)
	if !WarnDroppedTemplateKeys(withObs) {
		t.Fatal("第一次出现应当记")
	}
	if WarnDroppedTemplateKeys(withObs) {
		t.Fatal("同一组键不该重复记")
	}
	if !WarnDroppedTemplateKeys([]byte(`{"observatory":{},"metrics":{}}`)) {
		t.Fatal("键变了应当再记")
	}
	if WarnDroppedTemplateKeys([]byte(`{"log":{}}`)) {
		t.Fatal("没有被丢的键时不该记")
	}
	if !WarnDroppedTemplateKeys(withObs) {
		t.Fatal("修好之后再出现应当再记")
	}
}
