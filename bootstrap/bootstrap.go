// Package bootstrap 是安装脚本写入面板配置的唯一入口。
//
// 安装脚本不直接操作 SQLite：入站落库前要过 InboundService 的 xray 校验，
// settings/streamSettings 的 JSON 结构由前端模型定义，schema 也会随版本
// 变化——脚本手写这些只会在某次重启 xray 时静默失效。
package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"

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

	// 监听地址放在最后写：改成 127.0.0.1 之后面板就只能经由 Caddy 访问，
	// 前面任何一步失败都必须保持原样，否则会把管理员锁在门外。
	if err := s.SetListen(opts.Listen); err != nil {
		return nil, fmt.Errorf("写入监听地址失败: %w", err)
	}

	return &Result{Mode: opts.Mode, PanelURL: panelURL(opts)}, nil
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
