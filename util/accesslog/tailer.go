package accesslog

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Tailer 按偏移增量读取访问日志文件。
//
// 它有意做得很笨：每次调用打开、读、关闭，不持有长期文件句柄。日志采集
// 是几秒一次的低频动作，换来的是不用处理句柄失效、xray 重启、文件被外部
// 替换等一堆边界情况。
type Tailer struct {
	Path string

	offset int64
}

// Read 读取自上次调用以来新增的完整行并推进偏移，最多读 maxBytes 字节。
// 等价于 Peek 之后立刻 Commit；需要「写库成功才推进」的调用方用 Peek/Commit。
func (t *Tailer) Read(maxBytes int64) ([]string, error) {
	lines, next, err := t.Peek(maxBytes)
	if err != nil {
		return nil, err
	}
	t.Commit(next)
	return lines, nil
}

// Peek 读取自当前偏移以来新增的完整行，最多读 maxBytes 字节，返回读完这些行之后的
// 偏移 next，但**不推进**——调用方把这些行落库成功之后再 Commit(next)。先推进再写库的话，
// 写库失败（例如访问日志库被锁）这一批就永久丢了。
//
// 两条约束：
//   - 只返回以换行结尾的完整行。文件末尾的半行留到下次——xray 正在写入时
//     读到半行是常态，当成完整行解析会丢记录。
//   - 文件比记录的偏移还短，说明被截断或重建了，偏移回到 0 重新开始。
//     不处理这一条的话，采集会在第一次截断之后静默停摆。
func (t *Tailer) Peek(maxBytes int64) (lines []string, next int64, err error) {
	f, err := os.Open(t.Path)
	if err != nil {
		// xray 还没写过第一行时文件不存在，这不是错误。
		if errors.Is(err, fs.ErrNotExist) {
			return nil, t.offset, nil
		}
		return nil, t.offset, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, t.offset, err
	}
	if st.Size() < t.offset {
		t.offset = 0
	}
	if st.Size() == t.offset {
		return nil, t.offset, nil
	}

	size := st.Size() - t.offset
	if size > maxBytes {
		size = maxBytes
	}
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, t.offset)
	if err != nil && err != io.EOF {
		return nil, t.offset, err
	}
	buf = buf[:n]

	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		// 一整段里连一个换行都没有：要么是半行，要么是超长的畸形行。
		// 都不推进偏移，等下一轮。
		return nil, t.offset, nil
	}
	next = t.offset + int64(end) + 1

	text := string(buf[:end])
	if text == "" {
		return nil, next, nil
	}
	return strings.Split(text, "\n"), next, nil
}

// Offset 返回当前偏移。
func (t *Tailer) Offset() int64 {
	return t.offset
}

// Commit 把偏移推进到 Peek 返回的 next。
func (t *Tailer) Commit(next int64) {
	t.offset = next
}

// headMaxBytes 是读取文件首行时最多读的字节数。xray 的访问日志行远短于它。
const headMaxBytes = 4096

// Head 返回文件的第一条完整行，用作「还是不是同一份文件」的身份：偏移持久化之后，
// 重启时要确认文件没在期间被截断后又写过——那种情况下文件长度可能已超过旧偏移，
// 只比长度认不出来。文件不存在或还没有一条完整行时返回空串。
func (t *Tailer) Head() (string, error) {
	f, err := os.Open(t.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	buf := make([]byte, headMaxBytes)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	end := bytes.IndexByte(buf[:n], '\n')
	if end < 0 {
		return "", nil
	}
	return string(buf[:end]), nil
}

// Resume 在进程重启后恢复上次持久化的偏移：只有文件首行仍是 head、且文件不比 offset 短时
// 才恢复并返回 true；否则偏移保持为 0 从头读——文件已经换过，旧偏移指向的是另一份内容，
// 宁可重复导入也不跳过没读过的行。
//
// 修复前偏移只在内存里：面板每次重启（a-ui.sh 的 geo 定时更新会 systemctl restart）都把
// 整份 access.log 从 0 重新导入一遍，访问日志库里出现大批重复行。
func (t *Tailer) Resume(offset int64, head string) (bool, error) {
	if head == "" || offset <= 0 {
		return false, nil
	}
	cur, err := t.Head()
	if err != nil {
		return false, err
	}
	if cur != head {
		return false, nil
	}
	st, err := os.Stat(t.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if st.Size() < offset {
		return false, nil
	}
	t.offset = offset
	return true, nil
}

// TruncateIfLargerThan 在文件超过 maxSize 时把它清空并把偏移归零，返回是否截断了。
//
// xray 以 O_APPEND 打开日志文件，截断后它会继续从 0 追加，不需要重启。
// 代价是可能丢掉正在写入的那一行——比让日志无限增长把磁盘写满划算。
// 调用方应当在成功消费完全部内容之后再调用它；没消费完就截断（兜底上限）的调用方要自己
// 记一条告警，那等于丢掉了没读的积压。
func (t *Tailer) TruncateIfLargerThan(maxSize int64) (bool, error) {
	st, err := os.Stat(t.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if st.Size() <= maxSize {
		return false, nil
	}
	if err := os.Truncate(t.Path, 0); err != nil {
		return false, err
	}
	t.offset = 0
	return true, nil
}
