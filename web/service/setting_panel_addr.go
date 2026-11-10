package service

import "a-ui/web/entity"

// PanelAddr 读出库里当前的面板地址三项（监听地址、端口、补齐过斜杠的根路径）。
func (s *SettingService) PanelAddr() (entity.PanelAddr, error) {
	listen, err := s.GetListen()
	if err != nil {
		return entity.PanelAddr{}, err
	}
	port, err := s.GetPort()
	if err != nil {
		return entity.PanelAddr{}, err
	}
	basePath, err := s.GetBasePath()
	if err != nil {
		return entity.PanelAddr{}, err
	}
	return entity.PanelAddr{Listen: listen, Port: port, BasePath: basePath}, nil
}

// ValidatePanelAddrChange 对照库里的当前值，判断把面板地址改成 next 之后面板还能
// 不能访问到、监听得起来（规则见 entity.CheckPanelAddrChange）。设置页保存
// （UpdateAllSetting，排在 CheckValid 之后）与 `a-ui setting` 命令行共用。
//
// 不放进 SetPort/SetListen/SetBasePath：`a-ui bootstrap -force`（a-ui 菜单「配置
// 域名与伪装站」重跑向导）正是在 webListen 已经是 127.0.0.1 的状态下改端口与根路径、
// 并同时重写 Caddyfile 的那条路径，放进 setter 会把这条唯一的合法改法也拦掉。
func (s *SettingService) ValidatePanelAddrChange(next entity.PanelAddr) error {
	cur, err := s.PanelAddr()
	if err != nil {
		return err
	}
	return entity.CheckPanelAddrChange(cur, next)
}
