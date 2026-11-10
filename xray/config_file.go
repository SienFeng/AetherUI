package xray

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"

	"a-ui/util/common"
)

// configFilePerm 是 bin/config.json 的权限：文件里有全部用户的 UUID、密码与 REALITY 私钥，
// 只能属主可读写。修复前按 fs.ModePerm 新建，umask 022 下落成 0755，同机任何用户都读得到。
const configFilePerm fs.FileMode = 0o600

// WriteConfigFile 把配置原子地写到 bin/config.json（先写同目录的临时文件，再改名就位）。
//
// 必须是原子的：os.WriteFile 先截断原文件再写，磁盘满（ENOSPC）、只读（EROFS）时留下的是半截
// 配置——而 RestartXray 那时已经停掉了正在跑的核心，新核心又读不了这份文件，整机断网。现在写失败
// 时原文件一个字节都不动，RestartXray 在停核心之前先调它，失败就保留旧核心继续跑。
//
// 改名换的是整个 inode，所以存量部署里那份 0755 的旧文件也会被收紧成 0600。
func WriteConfigFile(cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return common.NewErrorf("生成 xray 配置文件失败: %v", err)
	}
	if err := writeFileAtomic(GetConfigPath(), data, configFilePerm); err != nil {
		return common.NewErrorf("写入配置文件失败: %v", err)
	}
	return nil
}

// writeFileAtomic 先把 data 写进 path 同目录下的临时文件并落盘，再改名覆盖 path。
// 临时文件必须与目标同目录：os.Rename 跨文件系统会失败。任何一步失败都删掉临时文件。
func writeFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	// 先落盘再改名：否则断电后可能换上一个长度对、内容却是空洞的文件。
	if err = f.Sync(); err != nil {
		return err
	}
	// Close 的错误不能丢：延迟写回的失败只会在这里报出来。
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
