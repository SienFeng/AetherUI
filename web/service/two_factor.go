package service

import (
	"errors"
	"strconv"
	"sync"
	"time"

	"a-ui/util/totp"
)

// 两步验证的三个 key 都只经 getOptionalString 读写，刻意不进
// defaultValueMap 与 entity.AllSetting：进了 AllSetting 就会随
// /aui/setting/all 下发给浏览器，密钥一旦出了服务器，第二个因素就不存在了。
const (
	// 非空即表示两步验证已开启。
	twoFactorSecretKey = "twoFactorSecret"
	// 已生成、尚未用一个有效验证码确认过的密钥。
	twoFactorPendingKey = "twoFactorPending"
	// 上一次被接受的验证码所属的时间步，用于拒绝重放。
	twoFactorLastStepKey = "twoFactorLastStep"
)

const twoFactorIssuer = "AetherUI"

// twoFactorMu 串行化「读上次的时间步 → 校验 → 写回」。不加锁的话，同一个
// 验证码的两次并发提交都会读到旧的时间步、都通过。
var twoFactorMu sync.Mutex

// twoFactorNow 可在测试里替换。
var twoFactorNow = time.Now

type TwoFactorService struct {
	settingService SettingService
}

// Enabled 报告两步验证是否开启。
//
// 读库失败时返回错误而不是 false：登录路径据此拒绝登录。把「读不出来」
// 当成「没开」，一次库锁就能让第二个因素凭空消失。
func (s *TwoFactorService) Enabled() (bool, error) {
	secret, err := s.settingService.getOptionalString(twoFactorSecretKey)
	if err != nil {
		return false, err
	}
	return secret != "", nil
}

// Begin 生成一个待确认的密钥，返回密钥文本与验证器扫码用的地址。
// 已开启时拒绝：否则任何拿到会话的人都能悄悄换掉密钥。
func (s *TwoFactorService) Begin(account string) (string, string, error) {
	enabled, err := s.Enabled()
	if err != nil {
		return "", "", err
	}
	if enabled {
		return "", "", errors.New("两步验证已开启，如需更换验证器请先关闭")
	}
	secret, err := totp.NewSecret()
	if err != nil {
		return "", "", err
	}
	if err := s.settingService.saveSetting(twoFactorPendingKey, secret); err != nil {
		return "", "", err
	}
	return secret, totp.URI(twoFactorIssuer, account, secret), nil
}

// Enable 用一个有效验证码确认待定密钥并正式开启。
//
// 必须先验证再开启：扫码失败或验证器时间不对的话，开启之后第一次登录
// 就会把管理员锁在门外。
func (s *TwoFactorService) Enable(code string) error {
	twoFactorMu.Lock()
	defer twoFactorMu.Unlock()

	enabled, err := s.Enabled()
	if err != nil {
		return err
	}
	if enabled {
		return errors.New("两步验证已开启")
	}
	pending, err := s.settingService.getOptionalString(twoFactorPendingKey)
	if err != nil {
		return err
	}
	if pending == "" {
		return errors.New("请先生成密钥")
	}
	step, ok := totp.Verify(pending, code, twoFactorNow())
	if !ok {
		return errors.New("验证码不正确，请确认验证器已添加该密钥且服务器时间准确")
	}
	// 先记时间步再写密钥：顺序反过来的话，两次写入之间失败会留下「已开启
	// 但没有重放记录」的状态，刚用过的这个码还能再登录一次。
	if err := s.settingService.saveSetting(twoFactorLastStepKey, strconv.FormatInt(step, 10)); err != nil {
		return err
	}
	if err := s.settingService.saveSetting(twoFactorSecretKey, pending); err != nil {
		return err
	}
	return s.settingService.saveSetting(twoFactorPendingKey, "")
}

// Disable 关闭两步验证并清掉全部相关状态。调用方负责先确认身份；
// `a-ui setting -disable2fa` 直接调用它，是丢失验证器时的救援入口。
func (s *TwoFactorService) Disable() error {
	twoFactorMu.Lock()
	defer twoFactorMu.Unlock()

	// 先清密钥：它是「是否开启」的唯一判据，清掉它之后即使后两步失败，
	// 状态也是一致的「已关闭」。
	for _, key := range []string{twoFactorSecretKey, twoFactorPendingKey, twoFactorLastStepKey} {
		if err := s.settingService.saveSetting(key, ""); err != nil {
			return err
		}
	}
	return nil
}

// Verify 校验一个验证码，通过后该码（及更早的码）不能再用。
func (s *TwoFactorService) Verify(code string) bool {
	twoFactorMu.Lock()
	defer twoFactorMu.Unlock()

	secret, err := s.settingService.getOptionalString(twoFactorSecretKey)
	if err != nil || secret == "" {
		return false
	}
	step, ok := totp.Verify(secret, code, twoFactorNow())
	if !ok {
		return false
	}
	last, err := s.settingService.getOptionalString(twoFactorLastStepKey)
	if err != nil {
		return false
	}
	if last != "" {
		lastStep, err := strconv.ParseInt(last, 10, 64)
		if err != nil || step <= lastStep {
			return false
		}
	}
	// 记不下时间步就不放行：否则这个码在有效期内可以被重放。
	if err := s.settingService.saveSetting(twoFactorLastStepKey, strconv.FormatInt(step, 10)); err != nil {
		return false
	}
	return true
}
