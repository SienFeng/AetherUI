// Package totp 实现 RFC 6238 的 TOTP（HMAC-SHA1、6 位、30 秒步长），
// 即 Google Authenticator / 1Password 等验证器的默认参数。
//
// 自己实现而不引入第三方库：算法就是一次 HMAC 加一次截断，标准库足够，
// 而这是认证链路，多一个依赖就多一份需要跟踪的供应链。
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	Period = 30
	Digits = 6
	// Skew 是允许的前后步数。验证器与服务器的时钟各自会漂，再加上用户
	// 读码到提交之间的几秒，±1 步（±30 秒）是通行取值。
	Skew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret 生成一个 160 位的随机密钥，返回 base32 文本（验证器要的格式）。
func NewSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

func decodeSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	s = strings.TrimRight(s, "=")
	key, err := b32.DecodeString(s)
	if err != nil || len(key) == 0 {
		return nil, fmt.Errorf("invalid totp secret")
	}
	return key, nil
}

// StepAt 返回某一时刻所在的时间步。
func StepAt(t time.Time) int64 {
	return t.Unix() / Period
}

func codeAtStep(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", Digits, bin%1000000)
}

// Code 返回某一时刻的验证码。
func Code(secret string, t time.Time) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return codeAtStep(key, StepAt(t)), nil
}

// Verify 在 ±Skew 步内比对验证码，命中时返回它所属的时间步。
//
// 返回时间步是为了让调用方拒绝重放：同一个码在它的有效期内（最长 90 秒）
// 可以被反复提交，调用方必须记住上一次接受的步，只接受更新的。
func Verify(secret string, code string, now time.Time) (int64, bool) {
	key, err := decodeSecret(secret)
	if err != nil {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != Digits {
		return 0, false
	}
	cur := StepAt(now)
	var matched int64
	ok := false
	// 不提前返回：三步都算完，耗时不随命中的是哪一步而变。
	for step := cur - Skew; step <= cur+Skew; step++ {
		if subtle.ConstantTimeCompare([]byte(codeAtStep(key, step)), []byte(code)) == 1 {
			matched, ok = step, true
		}
	}
	return matched, ok
}

// URI 生成验证器扫码用的 otpauth:// 地址。
func URI(issuer string, account string, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	return "otpauth://totp/" + label + "?" + q.Encode()
}
