// Package crypto 收口面板登录密码的哈希与比对。
//
// 所有会写 users.password 的路径都必须经 HashPassword：这一列一旦混进
// 明文，CheckPassword 会按「升级前留下的旧数据」放它登录（见下），界面上
// 完全看不出哪一行其实没被保护。
package crypto

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// MaxPasswordBytes 是 bcrypt 能处理的密码上限。超出的部分 bcrypt 算法
// 本身会直接截断丢弃，golang.org/x/crypto 选择报错而不是静默截断。
const MaxPasswordBytes = 72

var ErrPasswordTooLong = errors.New("密码不能超过 72 个字节（约 24 个汉字）")

// dummyHash 用于用户名不存在时仍然跑一次等价耗时的比对，见 BurnCheck。
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("a-ui-dummy-password"), bcrypt.DefaultCost)

func HashPassword(password string) (string, error) {
	if len(password) > MaxPasswordBytes {
		return "", ErrPasswordTooLong
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// IsHashed 判断库里的值是不是一个 bcrypt 哈希。
//
// 判据是 bcrypt 自己能否解析出 cost，而不是「以 $2 开头」：后者会把一个
// 恰好这样开头的明文密码误认成哈希，那个账号从此谁也登不上。一个明文
// 密码本身就是一条完整合法的 bcrypt 串仍会被误判，那只能靠
// `a-ui setting -username -password` 重置，概率上不值得为它加一列标记。
func IsHashed(stored string) bool {
	_, err := bcrypt.Cost([]byte(stored))
	return err == nil
}

// CheckPassword 比对密码。stored 不是哈希时按明文做常数时间比较。
//
// 明文分支只为一种数据存在：启动时的一次性迁移哈希不了的行（升级前设置的
// 密码超过 72 字节）。不留这条路，那样的管理员升级后会被直接锁在门外。
func CheckPassword(stored string, password string) bool {
	if IsHashed(stored) {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(password)) == 1
}

// BurnCheck 做一次结果必然为假的 bcrypt 比对。
//
// 用户名不存在时若直接返回，响应会比「用户名对、密码错」快出一次 bcrypt
// 的时间（约 50ms），攻击者据此就能枚举出管理员的用户名。
func BurnCheck(password string) {
	bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
}

// SessionFingerprint 由用户名与库里的密码值算出一个指纹，存进登录会话。
//
// 每次请求把会话里的指纹与库里现算的比一次，用户名或密码一变，此前签发
// 的所有会话当场失效——包括从别的机器、经 `a-ui setting -password` 改的。
// 指纹是哈希的哈希，从它推不回密码，也推不回库里的哈希。
func SessionFingerprint(username string, stored string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + stored))
	return hex.EncodeToString(sum[:16])
}
