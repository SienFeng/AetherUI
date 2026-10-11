package service

import (
	"path/filepath"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/crypto"
)

func setupUserDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "user.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

// setPlaintext 把首个用户的密码直接改成明文，模拟升级前的库。
func setPlaintext(t *testing.T, username string, password string) *model.User {
	t.Helper()
	db := database.GetDB()
	user := &model.User{}
	if err := db.First(user).Error; err != nil {
		t.Fatalf("取首个用户: %v", err)
	}
	err := db.Model(model.User{}).Where("id = ?", user.Id).
		Updates(map[string]any{"username": username, "password": password}).Error
	if err != nil {
		t.Fatalf("写入明文行: %v", err)
	}
	user.Username, user.Password = username, password
	return user
}

func storedPassword(t *testing.T) string {
	t.Helper()
	user := &model.User{}
	if err := database.GetDB().First(user).Error; err != nil {
		t.Fatalf("取首个用户: %v", err)
	}
	return user.Password
}

func TestDefaultAdminIsStoredHashed(t *testing.T) {
	setupUserDB(t)
	stored := storedPassword(t)
	if stored == "admin" || !crypto.IsHashed(stored) {
		t.Fatalf("默认账号的密码未哈希: %q", stored)
	}
	s := UserService{}
	if s.CheckUser("admin", "admin") == nil {
		t.Error("默认账号 admin/admin 无法登录")
	}
}

func TestMigratePlaintextPasswords(t *testing.T) {
	setupUserDB(t)
	setPlaintext(t, "root", "old-plain")
	s := UserService{}

	n, err := s.MigratePlaintextPasswords()
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n != 1 {
		t.Errorf("迁移行数 = %d, want 1", n)
	}
	stored := storedPassword(t)
	if stored == "old-plain" || !crypto.IsHashed(stored) {
		t.Fatalf("迁移后仍是明文: %q", stored)
	}
	if s.CheckUser("root", "old-plain") == nil {
		t.Error("迁移后原密码无法登录")
	}
	if s.CheckUser("root", stored) != nil {
		t.Error("迁移后拿哈希本身当密码竟能登录")
	}

	// 幂等：已是哈希的行不再动，否则每次启动都会把哈希再哈希一遍，
	// 第二次启动后谁也登不上。
	n, err = s.MigratePlaintextPasswords()
	if err != nil || n != 0 {
		t.Errorf("第二次迁移 n = %d, err = %v, want 0, nil", n, err)
	}
	if storedPassword(t) != stored {
		t.Error("第二次迁移改动了已哈希的行")
	}
}

// 升级前设置的超长密码哈希不了：必须原样保留并仍能登录，不能把人锁在门外。
func TestMigrateKeepsOverlongLegacyPasswordUsable(t *testing.T) {
	setupUserDB(t)
	long := strings.Repeat("p", crypto.MaxPasswordBytes+10)
	setPlaintext(t, "root", long)
	s := UserService{}

	n, err := s.MigratePlaintextPasswords()
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n != 0 {
		t.Errorf("迁移行数 = %d, want 0", n)
	}
	if storedPassword(t) != long {
		t.Error("超长密码行被改动了")
	}
	if s.CheckUser("root", long) == nil {
		t.Error("超长旧密码无法登录")
	}
}

func TestCheckUser(t *testing.T) {
	setupUserDB(t)
	s := UserService{}
	if err := s.UpdateFirstUser("alice", "correct horse"); err != nil {
		t.Fatalf("UpdateFirstUser: %v", err)
	}
	if got := storedPassword(t); got == "correct horse" || !crypto.IsHashed(got) {
		t.Fatalf("UpdateFirstUser 写入了明文: %q", got)
	}
	cases := []struct {
		name, username, password string
		want                     bool
	}{
		{"正确", "alice", "correct horse", true},
		{"密码错", "alice", "correct horsE", false},
		{"用户名错", "alicE", "correct horse", false},
		{"用户不存在", "bob", "correct horse", false},
		{"空密码", "alice", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.CheckUser(c.username, c.password) != nil; got != c.want {
				t.Errorf("CheckUser = %v, want %v", got, c.want)
			}
		})
	}
}

func TestUpdateUserStoresHashAndRejectsOverlong(t *testing.T) {
	setupUserDB(t)
	s := UserService{}
	first, err := s.GetFirstUser()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateUser(first.Id, "carol", "n3w-pass")
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	stored := storedPassword(t)
	if stored == "n3w-pass" || !crypto.IsHashed(stored) {
		t.Fatalf("UpdateUser 写入了明文: %q", stored)
	}
	if updated.Password != stored || updated.Username != "carol" {
		t.Errorf("返回值与库里不一致: %+v", updated)
	}
	if !s.VerifyPassword(first.Id, "n3w-pass") || s.VerifyPassword(first.Id, "admin") {
		t.Error("VerifyPassword 结果不对")
	}

	_, err = s.UpdateUser(first.Id, "carol", strings.Repeat("x", crypto.MaxPasswordBytes+1))
	if err != crypto.ErrPasswordTooLong {
		t.Errorf("超长密码 err = %v, want ErrPasswordTooLong", err)
	}
	if storedPassword(t) != stored {
		t.Error("被拒绝的修改仍然改动了库")
	}
}
