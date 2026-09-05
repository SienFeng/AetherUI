package service

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSelfSignedPair 生成一对能通过 tls.LoadX509KeyPair 的自签证书。
//
// 需要真证书而不是随便两个文件：CheckValid 对 WebCertFile/WebKeyFile 会真的
// 调用 tls.LoadX509KeyPair。若用假路径，「非回环监听 + 填了证书」的用例会因为
// 加载失败而报错，测试就分不清「被新规则拒了」还是「文件本来就不存在」——
// 而那恰恰是本文件要区分的两件事。
func writeSelfSignedPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "panel-tls-test"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31-1, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化密钥失败: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("写证书失败: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("写密钥失败: %v", err)
	}
	return certFile, keyFile
}

// 面板监听在回环地址，意味着它藏在前置反向代理后面（安装向导的 Caddy 拓扑）：
// TLS 由反代终结、以明文转发进来。此时若面板还自己监听 TLS，
// network.AutoHttpsConn 会把反代转发来的明文首包判成「非 TLS」，对每个请求回
// 一个 307 跳到同一个 URL —— 从外面看就是无限重定向，面板彻底打不开。
//
// 这个状态**没法从面板里救回来**（面板已经打不开了），重装也救不回来
// （bootstrap 靠 webBasePath != "/" 判定「已配置过」而整体跳过）。所以必须在
// 保存的那一刻就拒绝，而不是等它生效。
func TestCheckValidRejectsPanelCertOnLoopbackListen(t *testing.T) {
	certFile, keyFile := writeSelfSignedPair(t)

	cases := []struct {
		name   string
		listen string
		cert   string
		key    string
	}{
		{"IPv4 回环 + 完整证书对", "127.0.0.1", certFile, keyFile},
		{"IPv4 回环别名 + 完整证书对", "127.0.0.53", certFile, keyFile},
		{"IPv6 回环 + 完整证书对", "::1", certFile, keyFile},
		{"回环 + 只填了公钥", "127.0.0.1", certFile, ""},
		{"回环 + 只填了密钥", "127.0.0.1", "", keyFile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validBaseSetting()
			s.WebListen = c.listen
			s.WebCertFile = c.cert
			s.WebKeyFile = c.key

			err := s.CheckValid()
			if err == nil {
				t.Fatalf("监听 %q 且配置了面板证书，应当被拒绝，实际通过了", c.listen)
			}
			if !strings.Contains(err.Error(), "回环地址") {
				t.Fatalf("错误信息应指出是回环监听导致的，实际: %v", err)
			}
		})
	}
}

// 反面：面板直接对外暴露时（无域名安装的 REALITY 分支就是这样——install.sh 的
// reality_flow 不装任何反代，bootstrap 也不传 -listen，webListen 保持空串即
// 「监听所有 IP」），这两项是管理员给面板加 HTTPS 的唯一手段，绝不能误伤。
func TestCheckValidAllowsPanelCertOnPublicListen(t *testing.T) {
	certFile, keyFile := writeSelfSignedPair(t)

	for _, listen := range []string{"", "0.0.0.0", "10.0.0.5"} {
		name := listen
		if name == "" {
			name = "空串(所有 IP)"
		}
		t.Run(name, func(t *testing.T) {
			s := validBaseSetting()
			s.WebListen = listen
			s.WebCertFile = certFile
			s.WebKeyFile = keyFile

			if err := s.CheckValid(); err != nil {
				t.Fatalf("监听 %q 时配置面板证书是合法的，却被拒绝: %v", listen, err)
			}
		})
	}
}

// 回环监听但两项都留空，是 Caddy 拓扑下的正常状态，不能被这条规则误伤。
func TestCheckValidAllowsLoopbackListenWithoutPanelCert(t *testing.T) {
	s := validBaseSetting()
	s.WebListen = "127.0.0.1"
	s.WebCertFile = ""
	s.WebKeyFile = ""

	if err := s.CheckValid(); err != nil {
		t.Fatalf("回环监听 + 不配证书是 Caddy 拓扑的正常状态，却被拒绝: %v", err)
	}
}
