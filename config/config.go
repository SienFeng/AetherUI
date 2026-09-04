package config

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
)

//go:embed version
var version string

//go:embed name
var name string

type LogLevel string

const (
	Debug LogLevel = "debug"
	Info  LogLevel = "info"
	Warn  LogLevel = "warn"
	Error LogLevel = "error"
)

func GetVersion() string {
	return strings.TrimSpace(version)
}

func GetName() string {
	return strings.TrimSpace(name)
}

func GetLogLevel() LogLevel {
	if IsDebug() {
		return Debug
	}
	logLevel := os.Getenv("XUI_LOG_LEVEL")
	if logLevel == "" {
		return Info
	}
	return LogLevel(logLevel)
}

func IsDebug() bool {
	return os.Getenv("XUI_DEBUG") == "true"
}

func GetDBPath() string {
	return fmt.Sprintf("/etc/%s/%s.db", GetName(), GetName())
}

// GetAccessLogDBPath 是访问日志库的路径。与主库同目录但分文件，
// 便于单独删除或迁移。
func GetAccessLogDBPath() string {
	return fmt.Sprintf("/etc/%s/%s-access.db", GetName(), GetName())
}

// GetIPDBPath 是 ip2region 归属地库的落盘路径，GetQQWryPath 是纯真库那一路。
//
// 与主库同目录（/etc/<name>/），而不是安装目录下的 bin/：install.sh 更新面板时
// 会先 rm -rf 整个安装目录再铺开发版包，运行期下载生成的数据放在那里，每更新
// 一次就丢一次——纯真库直接消失显示「未下载」，ip2region 则被发版包里那份旧
// 构建静默换掉，界面照常显示「已加载」，数据其实已经退回去了。
func GetIPDBPath() string {
	return fmt.Sprintf("/etc/%s/ipdb.dat", GetName())
}

func GetQQWryPath() string {
	return fmt.Sprintf("/etc/%s/ipdb-qqwry.dat", GetName())
}
