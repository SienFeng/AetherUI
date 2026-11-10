package database

import (
	"os"
	"path/filepath"
	"testing"
)

// 三个库所在的目录（生产上是 /etc/<name>/）若由面板自己创建，权限必须是 0700。
//
// 改动前传给 MkdirAll 的是 fs.ModeDir，权限位为 0，建出来的目录是 d---------：
// 以 root 运行时 root 无视权限位，所以从来没人发现；换成非 root 运行（容器里、
// 本地开发）就连库文件都建不出来。目录里有 bcrypt 哈希、会话签名密钥和
// IP2Location 的 token，除面板自己之外没有任何进程需要进入它，所以是 0700
// 而不是 0750/0755。
func TestDatabaseDirectoriesAreCreatedPrivate(t *testing.T) {
	cases := []struct {
		name string
		init func(path string) error
	}{
		{"主库", InitDB},
		{"访问日志库", InitAccessLogDB},
		{"用量库", InitTrafficDB},
	}
	t.Cleanup(ResetTrafficDBForTest)
	t.Cleanup(resetAccessLogDBForTest)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "etc-a-ui")
			// 改动前建出来的是 0000 目录，TempDir 的清理会因为读不了目录而失败；
			// 先把权限放开，让失败只落在下面的断言上。
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			if err := c.init(filepath.Join(dir, "a-ui.db")); err != nil {
				t.Fatalf("在尚不存在的目录里初始化失败: %v", err)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Fatalf("目录权限 = %#o，期望 0700", perm)
			}
		})
	}
}

// resetAccessLogDBForTest 把访问日志库句柄清空，避免本包里的测试互相串用。
func resetAccessLogDBForTest() {
	sideDBMu.Lock()
	defer sideDBMu.Unlock()
	accessDB.Store(nil)
	accessDBPath = ""
}
