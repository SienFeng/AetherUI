package service

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// MaxCredentialBytes 是用户名（以及登录时提交的密码）的字节上限，登录接口
// （web/controller/index.go）用同一个值拒绝超长输入。写入路径必须同样设限：
// 存进一个超过上限的用户名，登录接口就会在查库之前把它当成超长输入拒掉，
// 管理员从此登不上。密码另有 bcrypt 的 72 字节上限（crypto.HashPassword）。
const MaxCredentialBytes = 256

// isTrimmedByLoginPage 对应浏览器 String.prototype.trim 会去掉的字符：
// unicode.IsSpace 覆盖 ASCII 空白、全角空格 U+3000 等 Z 类字符，BOM U+FEFF
// 不在其中，要单独补上。
func isTrimmedByLoginPage(r rune) bool {
	return unicode.IsSpace(r) || r == '\uFEFF'
}

// CheckNewCredential 校验即将写入的用户名与密码，设置页改账号（UpdateUser）与
// `a-ui setting -username -password`（UpdateFirstUser）共用。
//
// 拒绝首尾空白：登录页一直对输入做 trim，而这两条写入路径原样落库——一个末尾
// 带空格的密码存进去，登录时提交上来的永远少那个空格，管理员就此被锁在门外，
// 界面上却只看到「用户名或密码错误」。这类空格几乎都是复制粘贴时带进来的，
// 中间的空格不受影响。
func CheckNewCredential(username, password string) error {
	if username == "" {
		return errors.New("用户名不能为空")
	}
	if password == "" {
		return errors.New("密码不能为空")
	}
	if strings.TrimFunc(username, isTrimmedByLoginPage) != username {
		return errors.New("用户名首尾不能有空格（含全角空格、换行）：登录页会去掉首尾空格，存进去的用户名将永远对不上")
	}
	if strings.TrimFunc(password, isTrimmedByLoginPage) != password {
		return errors.New("密码首尾不能有空格（含全角空格、换行）：这类空格多半是复制时带进来的，登录时很容易对不上而被锁在面板外")
	}
	if len(username) > MaxCredentialBytes {
		return fmt.Errorf("用户名不能超过 %d 个字节", MaxCredentialBytes)
	}
	return nil
}
