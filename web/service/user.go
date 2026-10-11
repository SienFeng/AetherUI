package service

import (
	"errors"
	"gorm.io/gorm"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/crypto"
)

type UserService struct {
}

func (s *UserService) GetFirstUser() (*model.User, error) {
	db := database.GetDB()

	user := &model.User{}
	err := db.Model(model.User{}).
		First(user).
		Error
	if err != nil {
		return nil, err
	}
	return user, nil
}

// CheckUser 校验用户名与密码，通过时返回该用户。
//
// 先按用户名取行、再在 Go 侧比对哈希；用户名不存在时仍跑一次等价耗时的
// 比对（crypto.BurnCheck），否则响应时间会泄露「这个用户名存在」。
func (s *UserService) CheckUser(username string, password string) *model.User {
	db := database.GetDB()

	user := &model.User{}
	err := db.Model(model.User{}).
		Where("username = ?", username).
		First(user).
		Error
	if err == gorm.ErrRecordNotFound {
		crypto.BurnCheck(password)
		return nil
	} else if err != nil {
		logger.Warning("check user err:", err)
		return nil
	}
	if !crypto.CheckPassword(user.Password, password) {
		return nil
	}
	return user
}

// VerifyPassword 校验某个用户的当前密码，用于改密码、开关两步验证这类
// 需要再次确认身份的操作。
func (s *UserService) VerifyPassword(id int, password string) bool {
	db := database.GetDB()
	user := &model.User{}
	if err := db.Model(model.User{}).Where("id = ?", id).First(user).Error; err != nil {
		crypto.BurnCheck(password)
		return false
	}
	return crypto.CheckPassword(user.Password, password)
}

// UpdateUser 修改用户名与密码，返回更新后的用户（调用方要据此刷新会话）。
func (s *UserService) UpdateUser(id int, username string, password string) (*model.User, error) {
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return nil, err
	}
	db := database.GetDB()
	err = db.Model(model.User{}).
		Where("id = ?", id).
		Updates(map[string]any{"username": username, "password": hash}).
		Error
	if err != nil {
		return nil, err
	}
	return &model.User{Id: id, Username: username, Password: hash}, nil
}

func (s *UserService) UpdateFirstUser(username string, password string) error {
	if username == "" {
		return errors.New("username can not be empty")
	} else if password == "" {
		return errors.New("password can not be empty")
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return err
	}
	db := database.GetDB()
	user := &model.User{}
	err = db.Model(model.User{}).First(user).Error
	if database.IsNotFound(err) {
		user.Username = username
		user.Password = hash
		return db.Model(model.User{}).Create(user).Error
	} else if err != nil {
		return err
	}
	user.Username = username
	user.Password = hash
	return db.Save(user).Error
}

// MigratePlaintextPasswords 把库里仍是明文的密码一次性换成哈希，返回换了几行。
//
// 在面板启动时调用，而不是等到下一次登录再「顺手升级」：后者意味着一个
// 长期不登录的账号永远以明文躺在库里。不放进 database.InitDB，是因为
// InitDB 也被 `a-ui setting -show` 这类只读子命令调用，它们不该写库。
//
// 哈希不了的行（升级前设置的密码超过 72 字节）原样保留并告警，登录时走
// crypto.CheckPassword 的明文分支——宁可留一行没保护的数据，也不能把
// 管理员锁在门外。
func (s *UserService) MigratePlaintextPasswords() (int, error) {
	db := database.GetDB()
	var users []*model.User
	if err := db.Model(model.User{}).Find(&users).Error; err != nil {
		return 0, err
	}
	migrated := 0
	for _, user := range users {
		if crypto.IsHashed(user.Password) {
			continue
		}
		hash, err := crypto.HashPassword(user.Password)
		if err != nil {
			logger.Warning("用户", user.Id, "的密码无法转换为哈希，仍以明文保存，请在面板里重设一次密码:", err)
			continue
		}
		// 带上旧值做条件：启动期间若恰好有 `a-ui setting -password` 改了
		// 这一行，不能拿旧明文算出的哈希把它盖回去。
		res := db.Model(model.User{}).
			Where("id = ? and password = ?", user.Id, user.Password).
			Update("password", hash)
		if res.Error != nil {
			return migrated, res.Error
		}
		migrated += int(res.RowsAffected)
	}
	return migrated, nil
}
