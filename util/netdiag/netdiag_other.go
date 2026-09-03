//go:build !linux

package netdiag

// Supported 表示本平台能否读取内核连接表。
const Supported = false

// Dump 在非 Linux 平台上一律返回 ErrUnsupported。绝不返回空列表——
// 那会让界面把"看不到"显示成"没人在线"。
func Dump(Family) ([]Conn, error) {
	return nil, ErrUnsupported
}

// Destroy 在非 Linux 平台上一律返回 ErrUnsupported。
func Destroy(Conn) error {
	return ErrUnsupported
}
