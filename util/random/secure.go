package random

import "crypto/rand"

// secureSource 是 SecureSeq 的随机源。做成变量只为测试能换成确定的字节流，
// 验证输出确实取自这里；生产代码不改它。
var secureSource = rand.Read

// SecureSeq 返回 n 个取自 [0-9a-zA-Z] 的字符，随机源是 crypto/rand。
//
// 凡是拿来当密钥用的随机串（会话签名密钥 settings.secret 就是）都必须走这里，
// 不能用同包的 Seq：那是 math/rand，输出可以从种子推出来——Go 1.24 之前本包
// init 里的 rand.Seed(time.Now().UnixNano()) 让它只取决于进程启动的那一刻，
// 而会话 cookie 只签名不加密，密钥被猜中就能伪造任意用户的登录会话。
//
// 用拒绝采样而不是直接 b % 62：256 不是 62 的整数倍，直接取模会让前 8 个字符
// 出现得更频繁。只接受 [0, 248) 的字节（248 = 62 × 4），偏差为零。
func SecureSeq(n int) string {
	const limit = byte(len(allSeq) * 4)
	out := make([]rune, 0, n)
	buf := make([]byte, n+8)
	for len(out) < n {
		if _, err := secureSource(buf); err != nil {
			// crypto/rand 读不出来意味着系统熵源坏了。拿不到安全的随机数就不能
			// 生成密钥，退回弱随机源只会制造一把看起来正常、实际能被猜中的钥匙。
			panic("crypto/rand 不可用，无法生成密钥: " + err.Error())
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out = append(out, allSeq[int(b)%len(allSeq)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}
