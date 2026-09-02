package link

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// parseSocks parses socks share links. The format is not standardized, so we
// try three shapes in order:
//
//	1. socks://<base64(user:pass)>@host:port#remark   (v2rayN)
//	2. socks://<base64(user:pass@host:port)>#remark   (legacy)
//	3. socks5://user:pass@host:port#remark            (plain, credentials optional)
func parseSocks(rawLink string) (*ParseResult, error) {
	rest := rawLink
	for _, scheme := range []string{"socks5://", "socks4a://", "socks4://", "socks://"} {
		if strings.HasPrefix(rest, scheme) {
			rest = strings.TrimPrefix(rest, scheme)
			break
		}
	}

	remark := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		remark = decodeHash(rest[i+1:])
		rest = rest[:i]
	}
	if rest == "" {
		return nil, fmt.Errorf("socks: empty body")
	}

	// Shape 2: whole body is base64 and decodes to something with credentials.
	if !strings.Contains(rest, "@") {
		if dec, err := base64DecodeFlexible(rest); err == nil && strings.Contains(dec, "@") {
			rest = dec
		}
	}

	cred, hostport := "", rest
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		cred, hostport = rest[:at], rest[at+1:]
	}

	// Shape 1: the credential part itself may be base64. A base64 blob never
	// contains ":", so its absence is the signal to try decoding.
	if cred != "" && !strings.Contains(cred, ":") {
		if dec, err := base64DecodeFlexible(cred); err == nil && strings.Contains(dec, ":") {
			cred = dec
		}
	}

	user, pass := "", ""
	if cred != "" {
		if i := strings.Index(cred, ":"); i >= 0 {
			user, pass = cred[:i], cred[i+1:]
		} else {
			user = cred
		}
	}

	// net.SplitHostPort handles the [::1]:1080 bracket form correctly.
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		host, portStr = strings.Trim(hostport, "[]"), ""
	}
	if host == "" {
		return nil, fmt.Errorf("socks: missing host")
	}
	port := defaultPort(portStr, 1080)

	server := map[string]any{"address": host, "port": port}
	if user != "" || pass != "" {
		server["users"] = []any{map[string]any{"user": user, "pass": pass}}
	}

	ob := Outbound{
		"protocol":       "socks",
		"tag":            remark,
		"settings":       map[string]any{"servers": []any{server}},
		"streamSettings": buildStream("tcp", "none"),
	}
	identity := "socks:" + user + "@" + host + ":" + strconv.Itoa(port)
	return &ParseResult{Outbound: ob, Identity: identity}, nil
}
