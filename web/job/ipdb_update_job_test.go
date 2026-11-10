package job

import (
	"errors"
	"testing"

	"a-ui/web/service"
)

// 一部分源换上了新库、另一部分失败：允许集按库现算，换了库就得重新下发——不能因为有源失败就
// 连重启标志一起丢掉（修复前出错直接 return，核心一直用旧允许集）。
func TestAfterIPDBUpdateRestartsOnPartialSuccess(t *testing.T) {
	var xs service.XrayService
	xs.IsNeedRestartAndSetFalse()
	afterIPDBUpdate(&xs, "测试", 1, errors.New("第二个源失败"))
	if !xs.IsNeedRestartAndSetFalse() {
		t.Fatal("有源换了新库就要置重启标志")
	}
	afterIPDBUpdate(&xs, "测试", 0, errors.New("全部失败"))
	if xs.IsNeedRestartAndSetFalse() {
		t.Fatal("一个源都没换，不该置重启标志")
	}
}
