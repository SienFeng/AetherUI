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

// Read 读取自上次调用以来新增的完整行，最多读 maxBytes 字节。
//
// 两条约束：
//   - 只返回以换行结尾的完整行。文件末尾的半行留到下次——xray 正在写入时
//     读到半行是常态，当成完整行解析会丢记录。
//   - 文件比记录的偏移还短，说明被截断或重建了，偏移回到 0 重新开始。
//     不处理这一条的话，采集会在第一次截断之后静默停摆。
func (t *Tailer) Read(maxBytes int64) ([]string, error) {
	f, err := os.Open(t.Path)
	if err != nil {
		// xray 还没写过第一行时文件不存在，这不是错误。
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < t.offset {
		t.offset = 0
	}
	if st.Size() == t.offset {
		return nil, nil
	}

	size := st.Size() - t.offset
	if size > maxBytes {
		size = maxBytes
	}
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, t.offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	buf = buf[:n]

	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		// 一整段里连一个换行都没有：要么是半行，要么是超长的畸形行。
		// 都不推进偏移，等下一轮。
		return nil, nil
	}
	t.offset += int64(end) + 1

	text := string(buf[:end])
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// TruncateIfLargerThan 在文件超过 maxSize 时把它清空并把偏移归零。
//
// xray 以 O_APPEND 打开日志文件，截断后它会继续从 0 追加，不需要重启。
// 代价是可能丢掉正在写入的那一行——比让日志无限增长把磁盘写满划算。
// 调用方应当在成功消费完全部内容之后再调用它。
func (t *Tailer) TruncateIfLargerThan(maxSize int64) error {
	st, err := os.Stat(t.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if st.Size() <= maxSize {
		return nil
	}
	if err := os.Truncate(t.Path, 0); err != nil {
		return err
	}
	t.offset = 0
	return nil
}
