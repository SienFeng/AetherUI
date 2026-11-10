package controller

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"

	"a-ui/logger"
)

// 统一回复出口把错误原样写进日志：「立即更新」部分失败、保存域名组时校验订阅地址失败，错误里带着完整的订阅地址，
// 查询串里的 token 就进了日志。给管理员看的回复照旧完整（是他自己填的地址），日志里去掉查询串与账号密码。
func TestJsonMsgObjRedactsURLsInLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	logger.InitLogger(logging.DEBUG)
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { io.Copy(&buf, r); close(done) }()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	jsonMsgObj(c, "更新订阅", nil, errors.New(`Get "https://user:pw@example.com/sub?token=SECRET-123": dial tcp: refused`))

	w.Close()
	<-done
	os.Stderr = old
	logger.InitLogger(logging.INFO)
	out := buf.String()
	if !strings.Contains(out, "更新订阅失败") {
		t.Fatalf("没有捕获到日志: %q", out)
	}
	if strings.Contains(out, "SECRET-123") || strings.Contains(out, "user:pw") {
		t.Errorf("日志里带着订阅地址的凭据: %s", out)
	}
}
