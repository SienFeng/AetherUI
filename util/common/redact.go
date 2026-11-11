package common

import (
	"regexp"
	"strings"
)

// urlInText 匹配文本里出现的 URL。止于空白、引号与尖括号：*url.Error 的格式是
// `Get "<地址>": <原因>`，地址被 %q 包在一对双引号里。
var urlInText = regexp.MustCompile("[A-Za-z][A-Za-z0-9+.\\-]*://[^\\s\"'<>`]+")

// redactedQuery 替换掉被隐去的查询串，让读日志的人知道这里原本有内容。
const redactedQuery = "?<已隐去>"

// RedactURLs 去掉文本里每个 URL 的查询串、片段与 userinfo，以及路径里看起来像令牌的段（MaskSecretPathSegments），
// 主机与其余路径原样保留。
//
// 下载地址里常带个人凭证：IP2Location 的 token 就在查询串里
// （...?token=<TOKEN>&file=DB3LITE），也有人把账号密码写进 userinfo。
// http.Client 的错误会把完整地址原样带出来，直接写进日志就等于把凭证交给任何
// 能看日志的人。主机与路径要留下：排查「为什么下载失败」靠的就是它们。
//
// 只做字符串切分，不走 url.Parse：要处理的恰恰常是「地址本身写坏了」的那条
// 解析错误，解析不了也得能隐去。重复处理是幂等的。
func RedactURLs(s string) string {
	return urlInText.ReplaceAllStringFunc(s, redactURL)
}

func redactURL(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	head, rest := raw[:i+3], raw[i+3:]
	authority, tail := rest, ""
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		authority, tail = rest[:j], rest[j:]
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if k := strings.IndexAny(tail, "?#"); k >= 0 && k < len(tail)-1 {
		tail = MaskSecretPathSegments(tail[:k]) + redactedQuery
	} else {
		tail = MaskSecretPathSegments(tail)
	}
	return head + authority + tail
}

// redactedSegment 替换掉路径里看起来像令牌的一段。
const redactedSegment = "<已隐去>"

// secretSegment 是「看起来像令牌」的路径段：至少 16 个字符，全是令牌常用的字符（字母数字与 - _ = + ~），
// 而且字母、数字都有。机场订阅常把令牌放在路径里（/sub/<token>、/api/v1/client/<uuid>），只隐去查询串的话
// 它照样进日志。普通的路径段（subscribe、v1、geoip.dat 这类短词或带扩展名的文件名）原样保留，排查要靠它们。
var secretSegment = regexp.MustCompile(`^[A-Za-z0-9_=+~-]{16,}$`)

// MaskSecretPathSegments 把路径里看起来像令牌的段换成占位符，其余原样保留。
func MaskSecretPathSegments(path string) string {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if secretSegment.MatchString(seg) && strings.ContainsAny(seg, "0123456789") &&
			strings.IndexFunc(seg, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }) >= 0 {
			segs[i] = redactedSegment
		}
	}
	return strings.Join(segs, "/")
}

// RedactURLError 返回一个文本已经过 RedactURLs 处理的错误；文本里没有要隐去的
// 内容时原样返回 err。
//
// Unwrap 保留原错误链，errors.Is 判超时、判上游拒绝之类照常可用；但不要再把
// 解包出来的原错误打印出去，那就白隐去了。
func RedactURLError(err error) error {
	if err == nil {
		return nil
	}
	msg := RedactURLs(err.Error())
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Unwrap() error { return e.err }
