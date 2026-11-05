package xray

import (
	"strings"
	"testing"
)

func isMeter(tag string) bool { return strings.HasPrefix(tag, "a-ui-meter-") }

func cfgWith(routing string) *Config {
	return &Config{RouterConfig: []byte(routing)}
}

func TestOnlyTouchesTags(t *testing.T) {
	base := `{"domainStrategy":"IPIfNonMatch","rules":[{"type":"field","domain":["domain:cn"],"outboundTag":"a-ui-sg"},{"type":"field","domain":["domain:a.com"],"outboundTag":"a-ui-meter-1-a.com"}]}`
	meterSwap := `{"domainStrategy":"IPIfNonMatch","rules":[{"type":"field","domain":["domain:cn"],"outboundTag":"a-ui-sg"},{"type":"field","domain":["domain:b.com"],"outboundTag":"a-ui-meter-1-b.com"}]}`
	userChange := `{"domainStrategy":"IPIfNonMatch","rules":[{"type":"field","domain":["domain:cn","domain:x.com"],"outboundTag":"a-ui-sg"},{"type":"field","domain":["domain:b.com"],"outboundTag":"a-ui-meter-1-b.com"}]}`

	meterOutbounds := &HotDiff{
		RemovedOutboundTags: []string{"a-ui-meter-1-a.com"},
		AddedOutbounds:      [][]byte{[]byte(`{"tag":"a-ui-meter-1-b.com","protocol":"freedom"}`)},
		RoutingConfig:       []byte(meterSwap),
	}
	if !OnlyTouchesTags(cfgWith(base), cfgWith(meterSwap), meterOutbounds, isMeter) {
		t.Error("只换了计量出站与计量规则，应判为 true")
	}

	withUser := *meterOutbounds
	withUser.RoutingConfig = []byte(userChange)
	if OnlyTouchesTags(cfgWith(base), cfgWith(userChange), &withUser, isMeter) {
		t.Error("分流规则本身变了，应判为 false")
	}

	nodeAdded := &HotDiff{AddedOutbounds: [][]byte{[]byte(`{"tag":"a-ui-hk"}`)}}
	if OnlyTouchesTags(cfgWith(base), cfgWith(base), nodeAdded, isMeter) {
		t.Error("新增了普通出站，应判为 false")
	}

	inbound := &HotDiff{RemovedOutboundTags: []string{"a-ui-meter-1-a.com"}, RemovedInboundTags: []string{"inbound-1"}}
	if OnlyTouchesTags(cfgWith(base), cfgWith(base), inbound, isMeter) {
		t.Error("动了入站，应判为 false")
	}

	strategy := strings.Replace(meterSwap, "IPIfNonMatch", "AsIs", 1)
	d := *meterOutbounds
	d.RoutingConfig = []byte(strategy)
	if OnlyTouchesTags(cfgWith(base), cfgWith(strategy), &d, isMeter) {
		t.Error("routing 其他字段变了，应判为 false")
	}

	if OnlyTouchesTags(cfgWith(base), cfgWith(base), &HotDiff{}, isMeter) {
		t.Error("空 diff 不算")
	}
}
