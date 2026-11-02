package xray

import (
	"context"
	"testing"

	"github.com/xtls/xray-core/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsNoClueErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no clue over grpc", status.Error(codes.Unknown, common.ErrNoClue.Error()), true},
		{"other grpc error", status.Error(codes.Unknown, "other"), false},
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		if got := isNoClueErr(c.err); got != c.want {
			t.Errorf("%s: isNoClueErr = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTestRouteRequiresClient(t *testing.T) {
	var x XrayAPI
	if _, _, err := x.TestRoute(context.Background(), "inbound-1", "example.com", nil); err == nil {
		t.Fatal("未初始化的 XrayAPI 调用 TestRoute 应返回错误")
	}
}
