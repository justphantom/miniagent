package miniagent

import (
	"fmt"
	"net/url"
)

// ValidateURL 解析并校验 raw 为合法 http(s) URL（含 scheme+host）。
// core 的 config 校验与 provider 实现（internal/provider/openai）共用，避免分叉。
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("url %q 解析失败：%w（需 http(s)://host[:port][/path]）", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("url %q 缺少 scheme 或 host（应为 http(s)://host[:port]）", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url %q 的 scheme %q 不支持（仅 http/https）", raw, u.Scheme)
	}
	return u, nil
}
