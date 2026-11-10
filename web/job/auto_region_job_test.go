package job

import (
	"errors"
	"testing"

	"a-ui/web/service"
)

// IP 库没加载时任务每 10 分钟都会碰到同一个原因：只在开始时记一次，恢复之后再出现才再记，
// 不能每轮刷一行日志，也不能一行都不记。
func TestAutoRegionJobLogsMissingIPDBOnce(t *testing.T) {
	j := NewAutoRegionJob()
	steps := []struct {
		err  error
		want bool
	}{
		{service.ErrAutoRegionNoIPDB, true},
		{service.ErrAutoRegionNoIPDB, false},
		{service.ErrAutoRegionNoIPDB, false},
		{nil, false},
		{service.ErrAutoRegionNoIPDB, true},
		{errors.New("别的错误"), false},
		{service.ErrAutoRegionNoIPDB, true},
	}
	for i, s := range steps {
		if got := j.shouldLogNoIPDB(s.err); got != s.want {
			t.Errorf("第 %d 步 err=%v: got %v, want %v", i, s.err, got, s.want)
		}
	}
}
