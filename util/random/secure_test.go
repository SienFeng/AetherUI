package random

import (
	"errors"
	"strings"
	"testing"
)

func TestSecureSeqLengthAndCharset(t *testing.T) {
	for _, n := range []int{0, 1, 32, 200} {
		s := SecureSeq(n)
		if len(s) != n {
			t.Fatalf("SecureSeq(%d) 长度 = %d", n, len(s))
		}
		for _, r := range s {
			if !strings.ContainsRune(string(allSeq[:]), r) {
				t.Fatalf("SecureSeq 输出了字符集之外的字符 %q", r)
			}
		}
	}
	if SecureSeq(32) == SecureSeq(32) {
		t.Fatal("两次生成的 32 位密钥相同")
	}
}

// 输出必须取自 secureSource（生产上是 crypto/rand.Read），并且丢弃 >= 248 的
// 字节——直接取模会让前 8 个字符出现得更频繁。
func TestSecureSeqDrawsFromCryptoSourceWithoutModuloBias(t *testing.T) {
	old := secureSource
	t.Cleanup(func() { secureSource = old })
	feed := []byte{0, 61, 62, 247, 248, 255, 1}
	secureSource = func(b []byte) (int, error) {
		for i := range b {
			b[i] = feed[i%len(feed)]
		}
		return len(b), nil
	}
	// 0→'0'，61→'Z'，62→'0'，247→247%62=61→'Z'，248、255 丢弃，1→'1'
	if got, want := SecureSeq(5), "0Z0Z1"; got != want {
		t.Fatalf("SecureSeq(5) = %q，期望 %q", got, want)
	}

	secureSource = func([]byte) (int, error) { return 0, errors.New("熵源不可用") }
	defer func() {
		if recover() == nil {
			t.Fatal("crypto/rand 失败时必须 panic，而不是退回弱随机源或返回空串")
		}
	}()
	SecureSeq(8)
}
