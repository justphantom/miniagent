package miniagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	maxFetchBytes     = 200000 // body 上限 200KB
	fetchTimeout      = 20 * time.Second
	maxFetchRedirects = 5
)

var (
	fetchScriptRe = regexp.MustCompile(`(?is)<script\b.*?</script>`)
	fetchStyleRe  = regexp.MustCompile(`(?is)<style\b.*?</style>`)
	fetchTagRe    = regexp.MustCompile(`<[^>]*>`)
	fetchWSRe     = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

type fetchArgs struct {
	URL string `json:"url"`
}

// FetchTool 抓取 URL 转 plain text。SSRF：拒绝 loopback/私网/链路本地，限 http/https，
// 重定向上限 5 跳（每跳经 checkSSRF 重检）。彻底防护（DNS rebinding 等）仍需调用方
// 网络隔离，见 README「运行隔离」——free 模式下不在代码层做完整边界。
func FetchTool() Tool {
	return Tool{
		Name:        "fetch",
		Description: "抓取 http(s) URL 转 plain text（剥 script/style/标签）。SSRF 防护：拒绝 loopback/私网，重定向上限 5 跳。输出超 20000 字符截断，body 超 200KB 截断。用于查文档/查 API。",
		Parameters: object(map[string]any{
			"url": map[string]any{"type": "string", "description": "要抓取的 http(s) URL"},
		}, "url"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			return runFetch(ctx, args, checkSSRF)
		},
	}
}

// runFetch 抓取并清洗。ssrf 为 nil 时跳过目标检查（仅供测试绕过 loopback httptest）。
func runFetch(ctx context.Context, args string, ssrf func(context.Context, *url.URL) error) ToolResult {
	var a fetchArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if strings.TrimSpace(a.URL) == "" {
		return ToolResult{IsError: true, Output: "参数缺失：url"}
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("URL 非法：%v", err)}
	}
	if ssrf != nil {
		if err := ssrf(ctx, u); err != nil {
			return ToolResult{IsError: true, Output: err.Error()}
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	client := &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxFetchRedirects {
				return fmt.Errorf("重定向超过 %d 跳", maxFetchRedirects)
			}
			if ssrf != nil {
				return ssrf(req.Context(), req.URL)
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, a.URL, nil)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("构造请求失败：%v", err)}
	}
	req.Header.Set("User-Agent", "miniagent/fetch")
	resp, err := client.Do(req)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("抓取失败：%v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ToolResult{IsError: true, Output: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取失败：%v", err)}
	}
	overByte := len(body) > maxFetchBytes
	text := string(body)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		text = htmlToText(text)
	}
	out := truncate(text, maxShellOutputChars, "…[fetch 输出已截断]")
	if overByte {
		out += "\n…（body 超过 200KB，已截断）"
	}
	return ToolResult{Output: out}
}

// checkSSRF 限 http/https 且拒绝 loopback/私网/链路本地/未指定地址。
// 注意：DNS 解析（LookupIPAddr）与实际连接分离，无法防 rebinding——彻底防护需调用方
// 网络隔离。MVP 拦截静态内网/本地目标与非法 scheme。
func checkSSRF(ctx context.Context, u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅支持 http/https，got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL 缺 host")
	}
	ips, err := (&net.Resolver{}).LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("解析 %s 失败：%w", host, err)
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsUnspecified() {
			return fmt.Errorf("拒绝内网/本地地址 %s → %s", host, ip.IP)
		}
	}
	return nil
}

// htmlToText 最小清洗：去 script/style/标签，反转义实体，压缩多余空行。
// 不引入 x/net/html（第三方），正则对结构化文档页够用，复杂 SPA 有限。
func htmlToText(s string) string {
	s = fetchScriptRe.ReplaceAllString(s, " ")
	s = fetchStyleRe.ReplaceAllString(s, " ")
	s = fetchTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = fetchWSRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
