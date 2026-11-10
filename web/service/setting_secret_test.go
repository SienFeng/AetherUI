package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// 会话签名密钥（settings 表的 secret）必须出自 crypto/rand。
//
// 会话 cookie 只签名不加密，拿到这把密钥就能给任意用户伪造一个已登录的会话。
// 它的默认值在包初始化时生成一次、首次读取时落库固化，此后不再变化——生成
// 那一刻用的随机源决定了它能不能被猜出来。math/rand 不是密码学随机源，
// Go 1.24 之前 util/random 里那句 rand.Seed(time.Now().UnixNano()) 还让它
// 只取决于进程启动时刻。
//
// 随机源没法从输出上测出来，所以直接核对 defaultValueMap 的源码：secret 这一项
// 必须调用 random.SecureSeq（util/random/secure.go）。
func TestSessionSecretDefaultUsesCryptoRand(t *testing.T) {
	// 本包的 TestMain 会 chdir 到仓库根，按本文件所在目录定位 setting.go。
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("取不到测试文件路径")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(filepath.Dir(thisFile), "setting.go"), nil, 0)
	if err != nil {
		t.Fatalf("解析 setting.go: %v", err)
	}
	var found *ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			return true
		}
		if k, _ := strconv.Unquote(key.Value); k == "secret" {
			found, _ = kv.Value.(*ast.CallExpr)
		}
		return true
	})
	if found == nil {
		t.Fatal("defaultValueMap 里找不到由函数调用生成的 \"secret\" 默认值")
	}
	var pkgName, funcName string
	if sel, ok := found.Fun.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok {
			pkgName, funcName = pkg.Name, sel.Sel.Name
		}
	}
	if pkgName != "random" || funcName != "SecureSeq" {
		t.Fatalf("secret 的默认值必须由 random.SecureSeq（crypto/rand）生成，实际是 %s（%s）",
			types.ExprString(found), fset.Position(found.Pos()))
	}
}
