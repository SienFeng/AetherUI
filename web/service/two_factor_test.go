package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"a-ui/util/totp"
)

// freezeTwoFactorClock 把两步验证的时钟固定住，返回一个推进它的函数。
func freezeTwoFactorClock(t *testing.T) func(d time.Duration) time.Time {
	t.Helper()
	now := time.Unix(1700000000, 0)
	old := twoFactorNow
	twoFactorNow = func() time.Time { return now }
	t.Cleanup(func() { twoFactorNow = old })
	return func(d time.Duration) time.Time {
		now = now.Add(d)
		return now
	}
}

func mustCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.Code(secret, at)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	return code
}

func TestTwoFactorLifecycle(t *testing.T) {
	setupUserDB(t)
	advance := freezeTwoFactorClock(t)
	s := TwoFactorService{}

	if enabled, err := s.Enabled(); err != nil || enabled {
		t.Fatalf("初始状态 enabled = %v, err = %v", enabled, err)
	}
	if s.Verify("123456") {
		t.Error("未开启时 Verify 通过了")
	}
	if err := s.Enable("123456"); err == nil {
		t.Error("没有待确认密钥时 Enable 成功了")
	}

	secret, uri, err := s.Begin("admin")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if secret == "" || uri == "" {
		t.Fatal("Begin 返回空值")
	}
	// 只生成、未确认时不算开启：否则扫码失败的管理员下次登录就被锁住。
	if enabled, _ := s.Enabled(); enabled {
		t.Fatal("未确认就已开启")
	}
	if err := s.Enable("000000"); err == nil {
		t.Fatal("错误验证码开启成功了")
	}
	if enabled, _ := s.Enabled(); enabled {
		t.Fatal("错误验证码之后变成了已开启")
	}

	now := advance(0)
	code := mustCode(t, secret, now)
	if err := s.Enable(code); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if enabled, _ := s.Enabled(); !enabled {
		t.Fatal("Enable 之后仍未开启")
	}
	// 开启时用掉的那个码不能再拿来登录。
	if s.Verify(code) {
		t.Error("开启时用过的验证码被再次接受")
	}
	if _, _, err := s.Begin("admin"); err == nil {
		t.Error("已开启时 Begin 成功了，密钥可被悄悄替换")
	}

	now = advance(totp.Period * time.Second)
	next := mustCode(t, secret, now)
	if !s.Verify(next) {
		t.Fatal("下一个时间步的验证码未通过")
	}
	if s.Verify(next) {
		t.Error("同一个验证码被接受了两次（重放）")
	}
	// 重放防护不能只挡「同一个码」：比上次更早的码也必须拒绝。
	if s.Verify(code) {
		t.Error("更早时间步的验证码被接受")
	}

	if err := s.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if enabled, _ := s.Enabled(); enabled {
		t.Fatal("Disable 之后仍是开启")
	}
	now = advance(totp.Period * time.Second)
	if s.Verify(mustCode(t, secret, now)) {
		t.Error("关闭之后旧密钥的验证码仍被接受")
	}
	// 关闭后可以重新走一遍开启流程，且拿到的是新密钥。
	secret2, _, err := s.Begin("admin")
	if err != nil {
		t.Fatalf("关闭后 Begin: %v", err)
	}
	if secret2 == secret {
		t.Error("重新开启时沿用了旧密钥")
	}
}

// 密钥绝不能出现在下发给前端的设置里。
func TestTwoFactorSecretIsNotExposedThroughAllSetting(t *testing.T) {
	setupUserDB(t)
	advance := freezeTwoFactorClock(t)
	s := TwoFactorService{}
	secret, _, err := s.Begin("admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enable(mustCode(t, secret, advance(0))); err != nil {
		t.Fatal(err)
	}
	all, err := (&SettingService{}).GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(raw)), secret) {
		t.Fatalf("AllSetting 的 JSON 里出现了两步验证密钥")
	}
}
