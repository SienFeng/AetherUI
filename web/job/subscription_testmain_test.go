package job

import (
	"os"
	"testing"

	"a-ui/web/service"
)

// 测试里的订阅服务都在 127.0.0.1 上，而拉订阅默认不许连本机与内网地址（见 service.subscriptionClient）。
func TestMain(m *testing.M) {
	service.AllowLocalSubscriptionsForTest()
	os.Exit(m.Run())
}
