// Package bootstrap 是安装脚本写入面板配置的唯一入口。
//
// 安装脚本不直接操作 SQLite：入站落库前要过 InboundService 的 xray 校验，
// settings/streamSettings 的 JSON 结构由前端模型定义，schema 也会随版本
// 变化——脚本手写这些只会在某次重启 xray 时静默失效。
package bootstrap

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"a-ui/web/service"
)

type Options struct {
	Mode        string // caddy | reality
	Domain      string // mode=caddy 必填
	BasePath    string
	Listen      string
	Port        int
	CertFile    string
	KeyFile     string
	RealityDest string // mode=reality 必填
	Force       bool
	JSON        bool
	// Check 只查询是否已初始化，不做任何写入。安装脚本用它做幂等探测——
	// 拿一次真实的 Run 去「试探」会在全新机器上真的写进去，把探测用的
	// 假域名变成实际配置。
	Check bool
}

type Result struct {
	Mode     string `json:"mode"`
	PanelURL string `json:"panelUrl"`
	Skipped  bool   `json:"skipped"`
	Reason   string `json:"reason,omitempty"`
}

// alreadyInitialized 判断面板是否已被本命令配置过。
//
// 判据是 webBasePath 不再是默认的 "/"：随机根路径是本流程必定写入的一项，
// 而全新安装时它一定是 "/"。不拿 webListen 判断——它默认就是空串，
// 与「监听所有 IP」这个合法配置无法区分。
func alreadyInitialized() (bool, error) {
	s := service.SettingService{}
	basePath, err := s.GetBasePath()
	if err != nil {
		return false, err
	}
	return basePath != "/", nil
}

func Run(opts Options) (*Result, error) {
	// Check 分支必须排在 validate 之前：探测时不带 -mode/-domain。
	if opts.Check {
		done, err := alreadyInitialized()
		if err != nil {
			return nil, err
		}
		return &Result{Skipped: done}, nil
	}

	if err := validate(opts); err != nil {
		return nil, err
	}

	if !opts.Force {
		done, err := alreadyInitialized()
		if err != nil {
			return nil, err
		}
		if done {
			return &Result{
				Mode:    opts.Mode,
				Skipped: true,
				Reason:  "面板已配置过，保留现有设置（需要覆盖请加 -force）",
			}, nil
		}
	}

	s := service.SettingService{}

	if opts.Port > 0 {
		if err := s.SetPort(opts.Port); err != nil {
			return nil, fmt.Errorf("写入面板端口失败: %w", err)
		}
	}
	if err := s.SetBasePath(opts.BasePath); err != nil {
		return nil, fmt.Errorf("写入面板根路径失败: %w", err)
	}
	if opts.Domain != "" {
		if err := s.SetDefaultDomain(opts.Domain); err != nil {
			return nil, fmt.Errorf("写入默认域名失败: %w", err)
		}
	}
	if opts.CertFile != "" {
		if err := s.SetDefaultCertFile(opts.CertFile); err != nil {
			return nil, fmt.Errorf("写入默认证书路径失败: %w", err)
		}
	}
	if opts.KeyFile != "" {
		if err := s.SetDefaultKeyFile(opts.KeyFile); err != nil {
			return nil, fmt.Errorf("写入默认密钥路径失败: %w", err)
		}
	}

	if opts.Mode == "reality" {
		if err := createRealityInbound(opts); err != nil {
			return nil, err
		}
	}

	// mode=caddy 必须清空面板自己的直连 TLS 证书（webCertFile/webKeyFile，
	// 与上面写入的 defaultCertFile/defaultKeyFile 是完全不同的两个字段：
	// 后者只是新建入站表单的默认填充值，前者是 web.Server.Start() 用来决定
	// 要不要给自己的监听端口包一层 tls.Listener 的开关）。Caddy 已经终结
	// TLS、以明文转发到面板，面板没有理由再自己监听 TLS：
	//   - 遗留证书文件若仍存在，面板会用 network.AutoHttpsConn 窥探首包，
	//     把 Caddy 转发来的明文连接误判为"非 TLS 连接"，对每个请求都回一个
	//     307 到原 URL，从外面看就是死循环打不开。
	//   - 遗留证书文件若已不存在（比"路径还在但过期"更常见），
	//     tls.LoadX509KeyPair 会直接失败，Server.Start() 返回 error，
	//     main.go 只 log 一行就 return，进程以退出码 0 结束——而
	//     a-ui.service 是 Type=simple 且没有配 Restart=，面板从此彻底不再
	//     监听任何地址。此时唯一被打印过的恢复命令
	//     `a-ui setting -listen ""` 救不回来：它只改 webListen，不碰这两项。
	// mode=reality 不清：那条路径面板本来就是直连暴露给外部，管理员可能
	// 确实需要给面板自己配 HTTPS，不能替他关掉。
	if opts.Mode == "caddy" {
		if err := s.SetCertFile(""); err != nil {
			return nil, fmt.Errorf("清空面板证书路径失败: %w", err)
		}
		if err := s.SetKeyFile(""); err != nil {
			return nil, fmt.Errorf("清空面板密钥路径失败: %w", err)
		}
	}

	// 监听地址放在最后写：改成 127.0.0.1 之后面板就只能经由 Caddy 访问，
	// 前面任何一步失败都必须保持原样，否则会把管理员锁在门外。
	if err := s.SetListen(opts.Listen); err != nil {
		return nil, fmt.Errorf("写入监听地址失败: %w", err)
	}

	return &Result{Mode: opts.Mode, PanelURL: panelURL(opts)}, nil
}

// createRealityInbound 走 InboundService.AddInbound 而不是直接写库：
// 它内部会用真实 xray 校验完整生成配置，这道防线挡住的正是「配置非法
// 导致整份 bin/config.json 加载失败、机器上全部用户一起断网」。
func createRealityInbound(opts Options) error {
	serverName, _, ok := strings.Cut(opts.RealityDest, ":")
	if !ok || serverName == "" {
		return fmt.Errorf("-reality-dest 应形如 www.example.com:443，实际 %q", opts.RealityDest)
	}

	serverService := service.ServerService{}
	keys, err := serverService.GetNewX25519Cert()
	if err != nil {
		return fmt.Errorf("生成 REALITY 密钥失败: %w", err)
	}
	privateKey, _ := keys["privateKey"].(string)
	publicKey, _ := keys["publicKey"].(string)
	if privateKey == "" || publicKey == "" {
		return fmt.Errorf("生成 REALITY 密钥失败: 返回了空值")
	}

	shortID, err := randomHex(8)
	if err != nil {
		return fmt.Errorf("生成 shortId 失败: %w", err)
	}

	inbound, err := BuildRealityInbound(RealityParams{
		Port:       443,
		UUID:       uuid.New().String(),
		PrivateKey: privateKey,
		PublicKey:  publicKey,
		ShortID:    shortID,
		Target:     opts.RealityDest,
		ServerName: serverName,
		Remark:     "REALITY-" + serverName,
	})
	if err != nil {
		return err
	}

	// InboundController.getInbounds 按 user_id 过滤（WHERE user_id = ?，
	// 取自登录会话）。不设 UserId 的话入站落库时是 0，全新安装的 admin
	// 用户 Id 是 1（initUser 在空表上创建的第一条记录），管理员登录后会
	// 看到一个空的入站列表——入站其实建好了、也在跑，只是在面板里"隐形"。
	// GetFirstUser 与 setting -username/-password 用的是同一个"首个用户
	// 即管理员"的既有约定（main.go 的 UpdateFirstUser）。
	userService := service.UserService{}
	user, err := userService.GetFirstUser()
	if err != nil {
		return fmt.Errorf("查询管理员账号失败: %w", err)
	}
	inbound.UserId = user.Id

	inboundService := service.InboundService{}
	if err := inboundService.AddInbound(inbound); err != nil {
		return fmt.Errorf("创建 REALITY 入站失败: %w", err)
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validate(opts Options) error {
	switch opts.Mode {
	case "caddy":
		if opts.Domain == "" {
			return fmt.Errorf("mode=caddy 需要 -domain")
		}
	case "reality":
		if opts.RealityDest == "" {
			return fmt.Errorf("mode=reality 需要 -reality-dest")
		}
	default:
		return fmt.Errorf("未知的 -mode: %v（应为 caddy 或 reality）", opts.Mode)
	}
	if opts.BasePath == "" {
		return fmt.Errorf("需要 -basepath")
	}
	return nil
}

func panelURL(opts Options) string {
	if opts.Mode == "caddy" {
		return fmt.Sprintf("https://%v%v", opts.Domain, normalizedBasePath(opts.BasePath))
	}
	return fmt.Sprintf("http://<服务器IP>:%v%v", opts.Port, normalizedBasePath(opts.BasePath))
}

func normalizedBasePath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	if p[len(p)-1] != '/' {
		p += "/"
	}
	return p
}

// Print 把结果输出给安装脚本。-json 时输出机器可读格式供 jq 取值。
func (r *Result) Print(asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return
	}
	if r.Skipped {
		fmt.Println("跳过:", r.Reason)
		return
	}
	fmt.Println("面板地址:", r.PanelURL)
}
