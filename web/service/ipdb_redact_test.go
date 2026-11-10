package service

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// IP2Location 的下载地址里带着管理员个人的 token（...?token=<TOKEN>&file=DB3LITE）。
// http.Client 的错误（*url.Error）会把完整地址连同查询串原样带出来，而这条错误
// 既被写进面板日志（UpdateNow / refresh 各一处），又经 jsonMsg 再记一遍、回传
// 给前端。token 是个人凭证，泄露进日志等于交给任何能看日志的人。
//
// 主机与路径要保留：排查「为什么下载失败」靠的就是它们。
func TestIPDBDownloadErrorsDoNotLeakSourceCredentials(t *testing.T) {
	setupDB(t)
	// 一个已经关掉的本地端口：连接被拒，错误里带着完整地址，不碰外网。
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	const token = "EXAMPLE-TOKEN-0000"
	url := base + "/download?token=" + token + "&file=DB3LITE"
	path := filepath.Join(t.TempDir(), "ip2location.dat")
	useTestSources(t, []ipdbSource{urlSource("ip2location", path, url)})

	s := &IPDBService{}
	_, err := s.fetchAndBuild(ipdbSourceList()[0], url, path)
	if err == nil {
		t.Fatal("连接一个关掉的端口居然成功了")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("下载失败的错误里带出了 token: %v", err)
	}
	if !strings.Contains(err.Error(), strings.TrimPrefix(base, "http://")+"/download") {
		t.Errorf("错误里应保留主机与路径以便排查，实际: %v", err)
	}

	// UpdateNow 返回给设置页（并被 jsonMsg 再记一次日志）的错误同样不能带。
	_, _, err = s.UpdateNow("")
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("UpdateNow 的错误应当存在且不含 token，实际: %v", err)
	}

	// 地址本身写坏时，http.NewRequest 的解析错误同样会原样带出整串。
	bad := "http://[::1/download?token=" + token
	if _, err := s.fetchAndBuild(ipdbSourceList()[0], bad, path); err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("地址解析失败的错误应当存在且不含 token，实际: %v", err)
	}
}
