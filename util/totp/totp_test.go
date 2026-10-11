package totp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA1 测试向量。原表是 8 位码，这里取其低 6 位。
func TestCodeMatchesRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		got, err := Code(secret, time.Unix(c.unix, 0))
		if err != nil {
			t.Fatalf("Code(%d): %v", c.unix, err)
		}
		if got != c.want {
			t.Errorf("Code(%d) = %s, want %s", c.unix, got, c.want)
		}
	}
}

func TestVerifyAcceptsAdjacentStepsOnly(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	for _, offset := range []int64{-1, 0, 1} {
		at := now.Add(time.Duration(offset*Period) * time.Second)
		code, _ := Code(secret, at)
		step, ok := Verify(secret, code, now)
		if !ok {
			t.Errorf("偏移 %d 步的码未通过", offset)
		}
		if step != StepAt(at) {
			t.Errorf("偏移 %d 步: step = %d, want %d", offset, step, StepAt(at))
		}
	}
	for _, offset := range []int64{-2, 2} {
		code, _ := Code(secret, now.Add(time.Duration(offset*Period)*time.Second))
		if _, ok := Verify(secret, code, now); ok {
			t.Errorf("偏移 %d 步的码通过了", offset)
		}
	}
}

func TestVerifyRejectsMalformedInput(t *testing.T) {
	secret, _ := NewSecret()
	now := time.Unix(1700000000, 0)
	good, _ := Code(secret, now)
	for _, code := range []string{"", "12345", "1234567", good + "0", "abcdef"} {
		if _, ok := Verify(secret, code, now); ok {
			t.Errorf("Verify(%q) 通过了", code)
		}
	}
	if _, ok := Verify("not base32 !!", good, now); ok {
		t.Error("非法密钥下通过了")
	}
	if _, ok := Verify("", good, now); ok {
		t.Error("空密钥下通过了")
	}
	// 验证器显示成「123 456」，带空格粘贴进来应当照常通过。
	if _, ok := Verify(secret, good[:3]+" "+good[3:], now); !ok {
		t.Error("带空格的码未通过")
	}
}

func TestNewSecretIsRandomAndDecodable(t *testing.T) {
	a, _ := NewSecret()
	b, _ := NewSecret()
	if a == b {
		t.Error("两次生成的密钥相同")
	}
	if len(a) != 32 {
		t.Errorf("密钥长度 = %d, want 32（160 位的 base32）", len(a))
	}
	if _, err := Code(a, time.Now()); err != nil {
		t.Errorf("生成的密钥无法解码: %v", err)
	}
}

func TestURI(t *testing.T) {
	got := URI("AetherUI", "admin user", "ABC")
	if !strings.HasPrefix(got, "otpauth://totp/AetherUI:admin%20user?") {
		t.Errorf("URI = %q", got)
	}
	if !strings.Contains(got, "secret=ABC") || !strings.Contains(got, "issuer=AetherUI") {
		t.Errorf("URI 缺少参数: %q", got)
	}
}
