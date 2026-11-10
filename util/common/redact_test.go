package common

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestRedactURLs(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			`下载 IP 库源数据失败: Get "https://www.ip2location.com/download?token=EXAMPLE-TOKEN&file=DB3LITE": dial tcp: lookup www.ip2location.com: no such host`,
			`下载 IP 库源数据失败: Get "https://www.ip2location.com/download?<已隐去>": dial tcp: lookup www.ip2location.com: no such host`,
		},
		{
			`Get "https://user:hunter2@mirror.example.com:8443/qqwry.dat": EOF`,
			`Get "https://mirror.example.com:8443/qqwry.dat": EOF`,
		},
		// 解析不了的地址（错误本身就是「地址写坏了」）同样要隐去。
		{
			`parse "http://[::1/download?token=EXAMPLE-TOKEN": missing ']' in host`,
			`parse "http://[::1/download?<已隐去>": missing ']' in host`,
		},
		{"a https://example.com/x?k=v b http://192.0.2.1/y#frag c",
			"a https://example.com/x?<已隐去> b http://192.0.2.1/y?<已隐去> c"},
		{"https://example.com/plain/path", "https://example.com/plain/path"},
		{"https://example.com?token=EXAMPLE-TOKEN", "https://example.com?<已隐去>"},
		{"no url here: dial tcp 192.0.2.1:443: i/o timeout", "no url here: dial tcp 192.0.2.1:443: i/o timeout"},
		{"", ""},
	}
	for _, c := range cases {
		got := RedactURLs(c.in)
		if got != c.want {
			t.Errorf("RedactURLs(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
		if again := RedactURLs(got); again != got {
			t.Errorf("RedactURLs 不幂等: %q -> %q", got, again)
		}
	}
}

func TestRedactURLErrorKeepsErrorChain(t *testing.T) {
	if RedactURLError(nil) != nil {
		t.Fatal("nil 应当原样返回 nil")
	}
	plain := errors.New("connection refused")
	if RedactURLError(plain) != plain {
		t.Error("没有可隐去的内容时应当原样返回同一个 error")
	}

	err := &url.Error{Op: "Get", URL: "https://download.example.com/f?token=EXAMPLE-TOKEN", Err: context.DeadlineExceeded}
	red := RedactURLError(err)
	if got := red.Error(); got != `Get "https://download.example.com/f?<已隐去>": context deadline exceeded` {
		t.Errorf("Error() = %q", got)
	}
	if !errors.Is(red, context.DeadlineExceeded) {
		t.Error("隐去之后 errors.Is 判超时失效了")
	}
	if got := fmt.Sprint(red); got != red.Error() {
		t.Errorf("fmt 打印出了未隐去的内容: %q", got)
	}
}
