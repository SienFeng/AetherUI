package crypto

import (
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("s3cret-密码")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "s3cret-密码" {
		t.Fatal("哈希结果与明文相同")
	}
	if !IsHashed(hash) {
		t.Errorf("IsHashed(%q) = false", hash)
	}
	if !CheckPassword(hash, "s3cret-密码") {
		t.Error("正确密码未通过")
	}
	if CheckPassword(hash, "s3cret-密码 ") {
		t.Error("错误密码通过了")
	}
}

func TestHashPasswordIsSalted(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Error("两次哈希结果相同，说明没有加盐")
	}
}

func TestHashPasswordRejectsOverlong(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("a", MaxPasswordBytes)); err != nil {
		t.Errorf("72 字节应当可用: %v", err)
	}
	if _, err := HashPassword(strings.Repeat("a", MaxPasswordBytes+1)); err != ErrPasswordTooLong {
		t.Errorf("73 字节 err = %v, want ErrPasswordTooLong", err)
	}
}

func TestIsHashedRejectsPlaintextLookalikes(t *testing.T) {
	for _, s := range []string{"", "admin", "$2a$", "$2a$10$tooshort", "$2nothash"} {
		if IsHashed(s) {
			t.Errorf("IsHashed(%q) = true", s)
		}
	}
}

// 迁移不了的超长旧密码仍按明文比对，否则那位管理员升级后会被锁在门外。
func TestCheckPasswordFallsBackToPlaintextForLegacyRows(t *testing.T) {
	legacy := strings.Repeat("x", MaxPasswordBytes+8)
	if !CheckPassword(legacy, legacy) {
		t.Error("旧明文行的正确密码未通过")
	}
	if CheckPassword(legacy, legacy+"y") {
		t.Error("旧明文行的错误密码通过了")
	}
}

// 存的是哈希时，把哈希本身当密码提交绝不能通过——否则拿到数据库的人
// 不用破解就能登录，哈希就白做了。
func TestCheckPasswordDoesNotAcceptTheHashItself(t *testing.T) {
	hash, _ := HashPassword("real")
	if CheckPassword(hash, hash) {
		t.Error("哈希本身被当成了正确密码")
	}
}
