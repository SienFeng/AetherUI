package main

import (
	"flag"
	"fmt"
	"github.com/op/go-logging"
	"log"
	"os"
	"os/signal"
	"syscall"
	_ "unsafe"
	"a-ui/config"
	"a-ui/database"
	"a-ui/logger"
	"a-ui/util/tcshape"
	"a-ui/v2ui"
	"a-ui/web"
	"a-ui/web/global"
	"a-ui/web/service"
)

func runWebServer() {
	log.Printf("%v %v", config.GetName(), config.GetVersion())

	switch config.GetLogLevel() {
	case config.Debug:
		logger.InitLogger(logging.DEBUG)
	case config.Info:
		logger.InitLogger(logging.INFO)
	case config.Warn:
		logger.InitLogger(logging.WARNING)
	case config.Error:
		logger.InitLogger(logging.ERROR)
	default:
		log.Fatal("unknown log level:", config.GetLogLevel())
	}

	err := database.InitDB(config.GetDBPath())
	if err != nil {
		log.Fatal(err)
	}

	var server *web.Server

	server = web.NewServer()
	global.SetWebServer(server)
	err = server.Start()
	if err != nil {
		log.Println(err)
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGKILL)
	for {
		sig := <-sigCh

		switch sig {
		case syscall.SIGHUP:
			err := server.Stop()
			if err != nil {
				logger.Warning("stop server err:", err)
			}
			server = web.NewServer()
			global.SetWebServer(server)
			err = server.Start()
			if err != nil {
				log.Println(err)
				return
			}
		default:
			server.Stop()
			return
		}
	}
}

func resetSetting() {
	err := database.InitDB(config.GetDBPath())
	if err != nil {
		fmt.Println(err)
		return
	}

	settingService := service.SettingService{}
	err = settingService.ResetSettings()
	if err != nil {
		fmt.Println("reset setting failed:", err)
	} else {
		fmt.Println("reset setting success")
	}
}

// settingFlags 是 `a-ui setting` 解析后的参数。
//
// Listen / BasePath 用指针：flag 包区分不了「没传 -listen」和「传了
// -listen ""」，两者都是空字符串，而这两种语义完全相反——前者要保持
// 原值不动，后者是面板被锁在 127.0.0.1 上时的救援入口（清空监听地址
// = 监听所有 IP）。靠 flag.Visit 遍历实际出现过的 flag 来区分。
type settingFlags struct {
	Reset    bool
	Port     int
	Username string
	Password string
	Listen   *string
	BasePath *string
}

func parseSettingFlags(args []string) (settingFlags, error) {
	cmd := flag.NewFlagSet("setting", flag.ContinueOnError)
	var f settingFlags
	var listen, basePath string
	cmd.BoolVar(&f.Reset, "reset", false, "reset all setting")
	cmd.IntVar(&f.Port, "port", 0, "set panel port")
	cmd.StringVar(&f.Username, "username", "", "set login username")
	cmd.StringVar(&f.Password, "password", "", "set login password")
	cmd.StringVar(&listen, "listen", "", "set panel listen ip, empty means all")
	cmd.StringVar(&basePath, "basepath", "", "set panel url base path")
	if err := cmd.Parse(args); err != nil {
		return f, err
	}
	cmd.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "listen":
			f.Listen = &listen
		case "basepath":
			f.BasePath = &basePath
		}
	})
	return f, nil
}

func updateSetting(f settingFlags) {
	err := database.InitDB(config.GetDBPath())
	if err != nil {
		fmt.Println(err)
		return
	}

	settingService := service.SettingService{}

	if f.Port > 0 {
		err := settingService.SetPort(f.Port)
		if err != nil {
			fmt.Println("set port failed:", err)
		} else {
			fmt.Printf("set port %v success\n", f.Port)
		}
	}
	if f.Listen != nil {
		err := settingService.SetListen(*f.Listen)
		if err != nil {
			fmt.Println("set listen failed:", err)
		} else if *f.Listen == "" {
			fmt.Println("set listen to all interfaces success")
		} else {
			fmt.Printf("set listen %v success\n", *f.Listen)
		}
	}
	if f.BasePath != nil {
		err := settingService.SetBasePath(*f.BasePath)
		if err != nil {
			fmt.Println("set base path failed:", err)
		} else {
			fmt.Printf("set base path %v success\n", *f.BasePath)
		}
	}
	if f.Username != "" || f.Password != "" {
		userService := service.UserService{}
		err := userService.UpdateFirstUser(f.Username, f.Password)
		if err != nil {
			fmt.Println("set username and password failed:", err)
		} else {
			fmt.Println("set username and password success")
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		runWebServer()
		return
	}

	var showVersion bool
	flag.BoolVar(&showVersion, "v", false, "show version")

	runCmd := flag.NewFlagSet("run", flag.ExitOnError)

	v2uiCmd := flag.NewFlagSet("v2-ui", flag.ExitOnError)
	var dbPath string
	v2uiCmd.StringVar(&dbPath, "db", "/etc/v2-ui/v2-ui.db", "set v2-ui db file path")

	oldUsage := flag.Usage
	flag.Usage = func() {
		oldUsage()
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("    run            run web panel")
		fmt.Println("    v2-ui          migrate form v2-ui")
		fmt.Println("    setting        set settings（-port/-username/-password/-listen/-basepath/-reset）")
		fmt.Println("    tc-clear       清除本面板下发的全部 tc 限速规则（网络被限速规则掐断时的救援入口）")
	}

	flag.Parse()
	if showVersion {
		fmt.Println(config.GetVersion())
		return
	}

	switch os.Args[1] {
	case "run":
		err := runCmd.Parse(os.Args[2:])
		if err != nil {
			fmt.Println(err)
			return
		}
		runWebServer()
	case "v2-ui":
		err := v2uiCmd.Parse(os.Args[2:])
		if err != nil {
			fmt.Println(err)
			return
		}
		err = v2ui.MigrateFromV2UI(dbPath)
		if err != nil {
			fmt.Println("migrate from v2-ui failed:", err)
		}
	case "tc-clear":
		clearTrafficShaping()
	case "setting":
		f, err := parseSettingFlags(os.Args[2:])
		if err != nil {
			fmt.Println(err)
			return
		}
		if f.Reset {
			resetSetting()
		} else {
			updateSetting(f)
		}
	default:
		fmt.Println("except 'run' or 'v2-ui' or 'setting' or 'tc-clear' subcommands")
		fmt.Println()
		runCmd.Usage()
		fmt.Println()
		v2uiCmd.Usage()
	}
}

// clearTrafficShaping 不启动面板、不连数据库，直接把本面板下发的 tc 规则拆掉。
//
// 存在的理由：限速规则一旦配错就可能掐断整机网络，那时面板根本访问不到，
// 界面上那个「清除全部限速」按钮就没用了。这条命令是从服务商控制台/串口
// 登录进来之后的救援入口，不依赖任何前置条件。
func clearTrafficShaping() {
	if !tcshape.Supported {
		fmt.Println("当前系统不支持 tc，无需清除")
		return
	}
	iface, err := tcshape.DetectInterface()
	if err != nil {
		// 探测不出网卡也要把 ifb 那半边拆掉，否则 ingress 重定向会一直挂着。
		fmt.Println("警告: 无法探测默认路由所在网卡:", err)
		fmt.Println("将只清除本面板专用的 ifb 网卡；如需清除主网卡上的规则，请手动执行:")
		fmt.Println("    tc qdisc del dev <网卡名> root")
		fmt.Println("    tc qdisc del dev <网卡名> ingress")
		iface = ""
	}
	cmds := tcshape.BuildTeardownPlan(iface)
	if len(cmds) == 0 {
		cmds = tcshape.BuildIfbTeardownPlan()
	}
	if err := tcshape.Run(cmds); err != nil {
		fmt.Println("清除限速规则失败:", err)
		return
	}
	fmt.Println("已清除本面板下发的全部限速规则")
	if iface != "" {
		fmt.Println("网卡:", iface)
	}
}
