package service

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/random"
	"a-ui/util/reflect_util"
	"a-ui/web/entity"
)

//go:embed config.json
var xrayTemplateConfig string

var defaultValueMap = map[string]string{
	"xrayTemplateConfig":     xrayTemplateConfig,
	"webListen":              "",
	"webPort":                "54321",
	"webCertFile":            "",
	"webKeyFile":             "",
	"secret":                 random.Seq(32),
	"webBasePath":            "/",
	"timeLocation":           "Asia/Shanghai",
	"subscriptionUpdateTime": "04:00",
	"ipdbSourceUrl":          "https://raw.githubusercontent.com/lionsoul2014/ip2region/master/data/ipv4_source.txt",
	"ipdbUpdateTime":         "",
	"qqwrySourceUrl":         "https://raw.githubusercontent.com/FW27623/qqwry/main/qqwry.dat",
	"accessLogEnable":        "0",
	"accessLogRetentionDays": "7",
	"concurrencyIdleTimeout": "120",
	"tcInterface":            "",
}

type SettingService struct {
}

func (s *SettingService) GetAllSetting() (*entity.AllSetting, error) {
	db := database.GetDB()
	settings := make([]*model.Setting, 0)
	err := db.Model(model.Setting{}).Find(&settings).Error
	if err != nil {
		return nil, err
	}
	allSetting := &entity.AllSetting{}
	t := reflect.TypeOf(allSetting).Elem()
	v := reflect.ValueOf(allSetting).Elem()
	fields := reflect_util.GetFields(t)

	setSetting := func(key, value string) (err error) {
		defer func() {
			panicErr := recover()
			if panicErr != nil {
				err = errors.New(fmt.Sprint(panicErr))
			}
		}()

		var found bool
		var field reflect.StructField
		for _, f := range fields {
			if f.Tag.Get("json") == key {
				field = f
				found = true
				break
			}
		}

		if !found {
			// 有些设置自动生成，不需要返回到前端给用户修改
			return nil
		}

		fieldV := v.FieldByName(field.Name)
		switch t := fieldV.Interface().(type) {
		case int:
			n, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				return err
			}
			fieldV.SetInt(n)
		case string:
			fieldV.SetString(value)
		default:
			return common.NewErrorf("unknown field %v type %v", key, t)
		}
		return
	}

	keyMap := map[string]bool{}
	for _, setting := range settings {
		err := setSetting(setting.Key, setting.Value)
		if err != nil {
			return nil, err
		}
		keyMap[setting.Key] = true
	}

	for key, value := range defaultValueMap {
		if keyMap[key] {
			continue
		}
		err := setSetting(key, value)
		if err != nil {
			return nil, err
		}
	}

	return allSetting, nil
}

func (s *SettingService) ResetSettings() error {
	db := database.GetDB()
	return db.Where("1 = 1").Delete(model.Setting{}).Error
}

func (s *SettingService) getSetting(key string) (*model.Setting, error) {
	db := database.GetDB()
	setting := &model.Setting{}
	err := db.Model(model.Setting{}).Where("key = ?", key).First(setting).Error
	if err != nil {
		return nil, err
	}
	return setting, nil
}

func (s *SettingService) saveSetting(key string, value string) error {
	setting, err := s.getSetting(key)
	db := database.GetDB()
	if database.IsNotFound(err) {
		return db.Create(&model.Setting{
			Key:   key,
			Value: value,
		}).Error
	} else if err != nil {
		return err
	}
	setting.Key = key
	setting.Value = value
	return db.Save(setting).Error
}

func (s *SettingService) getString(key string) (string, error) {
	setting, err := s.getSetting(key)
	if database.IsNotFound(err) {
		value, ok := defaultValueMap[key]
		if !ok {
			return "", common.NewErrorf("key <%v> not in defaultValueMap", key)
		}
		return value, nil
	} else if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (s *SettingService) setString(key string, value string) error {
	return s.saveSetting(key, value)
}

// getOptionalString 读一个可能从未写过的 key，未写过时返回空串。
//
// 与 getString 的区别是不要求 key 出现在 defaultValueMap 里：上游 ETag、
// 上次向上游确认的时间这类纯运行期状态没有「默认值」可言，把它们塞进
// defaultValueMap 只会让那张表看起来像是有语义的配置项。
func (s *SettingService) getOptionalString(key string) (string, error) {
	setting, err := s.getSetting(key)
	if database.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (s *SettingService) getInt(key string) (int, error) {
	str, err := s.getString(key)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(str)
}

func (s *SettingService) setInt(key string, value int) error {
	return s.setString(key, strconv.Itoa(value))
}

func (s *SettingService) GetXrayConfigTemplate() (string, error) {
	template, err := s.getString("xrayTemplateConfig")
	if err != nil {
		return "", err
	}

	// 补 RoutingService。存量部署的模板一旦被管理员改过就落进 settings 表，
	// 改默认值对它们无效，所以在读取路径上补，读一次补一次（幂等）。
	//
	// 补完立刻写回，而不是每次读都在内存里补：写回后管理员在设置页看到的
	// 模板与实际生效的一致；只在内存补的话，他保存一次就把 RoutingService
	// 弄丢了，而且丢得毫无提示。
	patched, changed, err := ensureRoutingServiceInTemplate(template)
	if err != nil {
		// 模板本来就不合法是既有问题，不是本次改动造成的。原样返回让
		// 后续流程按老路径报错，不要在这里把管理员锁在门外。
		logger.Warning("xray 模板无法解析，跳过 RoutingService 补齐:", err)
		return template, nil
	}
	if changed {
		if err := s.setString("xrayTemplateConfig", patched); err != nil {
			logger.Warning("RoutingService 补齐后写回失败，本次仅在内存生效:", err)
		} else {
			logger.Info("已为 xray 模板补上 RoutingService，路由热更新与路由测试现在可用")
		}
	}
	return patched, nil
}

// ensureRoutingServiceInTemplate 往模板的 api.services 里补上 RoutingService。
//
// 路由规则的热重载与路由测试都走 RoutingService，模板里不声明它，xray 就
// 不会起这个 gRPC 服务，功能会静默不可用——不报错，只是永远连不上。
//
// 只在 api 段已存在时补齐：api 段整个缺失说明管理员刻意关掉了控制接口，
// 那种情况下流量统计本来就是坏的，不该由这里替他做决定。
//
// 幂等：已含 RoutingService 时原样返回。api 之外的键不做任何改动——
// 反序列化成 map[string]json.RawMessage 再序列化，其余键按原始字节透传。
func ensureRoutingServiceInTemplate(template string) (string, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(template), &root); err != nil {
		return "", false, common.NewError("xray 模板不是合法 JSON:", err)
	}

	rawAPI, ok := root["api"]
	if !ok {
		return template, false, nil
	}

	var api map[string]json.RawMessage
	if err := json.Unmarshal(rawAPI, &api); err != nil {
		return "", false, common.NewError("xray 模板的 api 段不是合法 JSON 对象:", err)
	}

	var services []string
	if rawServices, ok := api["services"]; ok {
		if err := json.Unmarshal(rawServices, &services); err != nil {
			return "", false, common.NewError("xray 模板的 api.services 不是字符串数组:", err)
		}
	}
	for _, s := range services {
		if s == routingServiceName {
			return template, false, nil
		}
	}

	// 追加到末尾而不是插入：顺序对 xray 无意义，但保持原有顺序能让
	// 管理员对比前后模板时只看到多出的一行。
	services = append(services, routingServiceName)
	encoded, err := json.Marshal(services)
	if err != nil {
		return "", false, err
	}
	api["services"] = encoded

	encodedAPI, err := json.Marshal(api)
	if err != nil {
		return "", false, err
	}
	root["api"] = encodedAPI

	// 不能直接 json.Marshal(root)：root 的值是 json.RawMessage，标准库对
	// 任何实现了 MarshalJSON 的值都会在写出前跑一遍 compact()，结果是整份
	// 模板被压成不含空白的单行，还会把 <、>、& 转义成 \u003c 等（模板里的
	// outbound/订阅地址常带 & 的 URL）。这份串随后经 setString 落库，
	// GetAllSetting 又直接读回设置页——管理员保存过一次设置的部署（绝大
	// 多数部署，UpdateAllSetting 会把 xrayTemplateConfig 一起落库）升级后
	// 打开设置页就会看到模板变成一整行，是用户可见的编辑体验回退。
	//
	// 用 Encoder + SetEscapeHTML(false) 关掉转义；Encode 会在末尾追加一个
	// 换行，交给 json.Indent 重新缩进时一并被丢弃（Indent 只保留输入里
	// 合法 JSON 值对应的字节）。缩进两个空格，与内嵌默认模板
	// web/service/config.json 的风格一致。
	var compact bytes.Buffer
	enc := json.NewEncoder(&compact)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return "", false, err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, compact.Bytes(), "", "  "); err != nil {
		return "", false, err
	}
	return indented.String(), true, nil
}

// routingServiceName 是 xray api.services 里 RoutingService 的名字。
const routingServiceName = "RoutingService"

func (s *SettingService) GetListen() (string, error) {
	return s.getString("webListen")
}

func (s *SettingService) GetPort() (int, error) {
	return s.getInt("webPort")
}

func (s *SettingService) SetPort(port int) error {
	return s.setInt("webPort", port)
}

func (s *SettingService) GetCertFile() (string, error) {
	return s.getString("webCertFile")
}

func (s *SettingService) GetKeyFile() (string, error) {
	return s.getString("webKeyFile")
}

func (s *SettingService) GetSecret() ([]byte, error) {
	secret, err := s.getString("secret")
	if secret == defaultValueMap["secret"] {
		err := s.saveSetting("secret", secret)
		if err != nil {
			logger.Warning("save secret failed:", err)
		}
	}
	return []byte(secret), err
}

func (s *SettingService) GetBasePath() (string, error) {
	basePath, err := s.getString("webBasePath")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}
	return basePath, nil
}

func (s *SettingService) GetTimeLocation() (*time.Location, error) {
	l, err := s.getString("timeLocation")
	if err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(l)
	if err != nil {
		defaultLocation := defaultValueMap["timeLocation"]
		logger.Errorf("location <%v> not exist, using default location: %v", l, defaultLocation)
		return time.LoadLocation(defaultLocation)
	}
	return location, nil
}

// GetSubscriptionUpdateTime 返回域名组订阅的每日更新时刻，格式 HH:MM。
func (s *SettingService) GetSubscriptionUpdateTime() (string, error) {
	return s.getString("subscriptionUpdateTime")
}

// GetIPDBSourceUrl 返回 IP 归属地库的源数据地址。做成可配置是因为默认地址在
// GitHub，部分网络下不可达。
func (s *SettingService) GetIPDBSourceUrl() (string, error) {
	return s.getString("ipdbSourceUrl")
}

// GetQQWrySourceUrl 返回纯真 IP 库的下载地址。留空表示不启用该数据源。
//
// 纯真库是第二个离线数据源：中文原生、每天有镜像更新，与 ip2region 交叉校验。
func (s *SettingService) GetQQWrySourceUrl() (string, error) {
	return s.getString("qqwrySourceUrl")
}

// GetIPDBUpdateTime 返回 IP 库的每日更新时刻，格式 HH:MM，留空表示关闭自动更新。
//
// 用「每天几点」而不是「隔几天」：管理员能把它放进自己的低谷时段，
// 更新时机可预期，也不会出现「刚好在高峰期开始下 35 MB」这种事。
func (s *SettingService) GetIPDBUpdateTime() (string, error) {
	return s.getString("ipdbUpdateTime")
}

// GetAccessLogEnable 返回是否记录访问日志。
//
// 默认关闭：它是持续写盘的动作，记录的又是用户访问了哪些站点，
// 该由管理员显式打开而不是替他决定。
func (s *SettingService) GetAccessLogEnable() (bool, error) {
	v, err := s.getInt("accessLogEnable")
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// GetConcurrencyIdleTimeout 返回并发判定的闲置阈值（秒）。0 表示关闭闲置判定。
func (s *SettingService) GetConcurrencyIdleTimeout() (int, error) {
	return s.getInt("concurrencyIdleTimeout")
}

// GetAccessLogRetentionDays 返回访问日志保留天数。
func (s *SettingService) GetAccessLogRetentionDays() (int, error) {
	return s.getInt("accessLogRetentionDays")
}

// GetTCInterface 返回下发限速规则的网卡名。留空表示按默认路由自动探测。
//
// 做成可配置是因为多网卡机器（尤其是带隧道、带 NAT 的）上，默认路由所在的
// 网卡未必就是客户端流量真正进出的那块。
func (s *SettingService) GetTCInterface() (string, error) {
	return s.getString("tcInterface")
}

func (s *SettingService) UpdateAllSetting(allSetting *entity.AllSetting) error {
	if err := allSetting.CheckValid(); err != nil {
		return err
	}

	v := reflect.ValueOf(allSetting).Elem()
	t := reflect.TypeOf(allSetting).Elem()
	fields := reflect_util.GetFields(t)
	errs := make([]error, 0)
	for _, field := range fields {
		key := field.Tag.Get("json")
		fieldV := v.FieldByName(field.Name)
		value := fmt.Sprint(fieldV.Interface())
		err := s.saveSetting(key, value)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return common.Combine(errs...)
}
