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
	"sync"
	"syscall"
	"time"
)

const (
	maxFetchBytes     = 200000 // body 上限 200KB
	fetchTimeout      = 20 * time.Second
	maxFetchRedirects = 5
)

// fetchHeaderTimeout/fetchDialTimeout 在 Transport 层限制恶意超大响应头与慢连接建立：
// http.Client.Timeout(fetchTimeout) 按规范含 reading response body，但 header 阶段
// 无显式上限时被恶意识服务器拖延至整体 20s。用 var 而非 const 仅为测试可短化覆盖。
var (
	fetchHeaderTimeout = 10 * time.Second
	fetchDialTimeout   = 10 * time.Second
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
// 重定向上限 5 跳（每跳经 checkSSRF 重检）；DNS rebinding 经 custom DialContext 的 Control
// 钩子做 IP pin 闭合（checkSSRF 批准集 vs 连接二次解析的实际 IP）。彻底防护仍需调用方
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

// runFetch 抓取并清洗。ssrf==nil 时兜底用 checkSSRF：签名保留 nil 曾是维护陷阱
// （生产路径固定传 checkSSRF，但 nil 会静默旁路 SSRF）。测试需绕过 loopback httptest
// 时显式传入 no-op func，而非依赖 nil。
func runFetch(ctx context.Context, args string, ssrf func(context.Context, *url.URL) error) ToolResult {
	if ssrf == nil {
		ssrf = checkSSRF
	}
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
	// approvedSet 经 ctx 传给 custom DialContext 的 Control 钩子，闭合 DNS rebinding：
	// checkSSRF 把每个 host 的解析结果（批准 IP）记入 set，连接时 net 层二次解析若返回
	// 不同 IP（攻击者权威 DNS rebinding 到内网元数据端点），Control 拒绝。set 是指针，
	// 经 context.WithValue 沿重定向链传播（redirect req 继承 ireq.ctx）。
	approved := &approvedSet{ips: make(map[string][]net.IP)}
	ctx2 := context.WithValue(ctx, approvedIPsKey{}, approved)
	if err := ssrf(ctx2, u); err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	runCtx, cancel := context.WithTimeout(ctx2, fetchTimeout)
	defer cancel()
	client := &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			// Proxy 沿用 DefaultTransport 行为（HTTP_PROXY/HTTPS_PROXY），避免新增 Transport
			// 丢失代理支持（corporate 网络出网依赖）。
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: fetchHeaderTimeout,
			// 自定义 DialContext 闭合 DNS rebinding：从 ctx 取该 host 经 checkSSRF 批准的
			// IP 集，交给内层 Dialer 的 Control 钩子（connect 前触发，address 已是 net 层
			// 二次解析后的 IP:port）校验「实际拨号 IP ∈ 批准集」。checkSSRF 解析与连接解析
			// 由此原子化——攻击者权威 DNS 在二次解析返 169.254.169.254 时被 Control 拒绝。
			// 重定向每跳经 CheckRedirect 重跑 checkSSRF，补充该 host 的批准集（ireq.ctx 沿
			// 链传播，见 client.go redirect 构造）。
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, _, _ := net.SplitHostPort(address)
				var ips []net.IP
				if s, ok := ctx.Value(approvedIPsKey{}).(*approvedSet); ok && s != nil {
					s.mu.Lock()
					ips = s.ips[host]
					s.mu.Unlock()
				}
				d := &net.Dialer{
					Timeout: fetchDialTimeout,
					Control: func(network, address string, _ syscall.RawConn) error {
						return validateDialIP(ips, address)
					},
				}
				return d.DialContext(ctx, network, address)
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxFetchRedirects {
				return fmt.Errorf("重定向超过 %d 跳", maxFetchRedirects)
			}
			return ssrf(req.Context(), req.URL)
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

// checkSSRF 限 http/https 且拒绝 loopback/私网/链路本地/未指定地址。解析所得 IP 经
// 私网校验后记入 ctx 的 approvedSet，供 custom DialContext 的 Control 钩子 pin——
// 解析与连接二次解析由此原子化，闭合 DNS rebinding（见 runFetch/validateDialIP）。
// approvedSet 缺失（如 TestCheckSSRF 直接调用、或测试 noopSSRF 旁路）时仅跳过记录，
// scheme/IP 类校验仍完整执行。
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
	if s, ok := ctx.Value(approvedIPsKey{}).(*approvedSet); ok && s != nil {
		approved := make([]net.IP, len(ips))
		for i := range ips {
			approved[i] = ips[i].IP
		}
		s.mu.Lock()
		s.ips[host] = approved
		s.mu.Unlock()
	}
	return nil
}

// approvedIPsKey 是 context.WithValue 的键类型，承载 checkSSRF 批准的 host→IPs 集。
type approvedIPsKey struct{}

// approvedSet 记录每个 host 经 checkSSRF 批准的 IP。fetch 单请求链串行（初始 + 各重定向），
// checkSSRF 写、DialContext 读；mutex 防御 race 检测器（Transport 内部 dial 跨 goroutine）。
type approvedSet struct {
	mu  sync.Mutex
	ips map[string][]net.IP
}

// validateDialIP 校验 Control 钩子收到的实际远端 IP（address 形如 "1.2.3.4:80" 或
// "[::1]:80"，已由 net 层解析自 hostname）属于 checkSSRF 批准集。approved 为空（checkSSRF
// 旁路，如测试 noop）时放行——rebinding 防护是 checkSSRF 之上的纵深层，仅在 checkSSRF
// 已记录批准集时生效；生产路径 checkSSRF 必跑，到达拨号时该 host 的集必非空。
func validateDialIP(approved []net.IP, address string) error {
	if len(approved) == 0 {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("fetch: 拨号地址非法 %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("fetch: 拨号地址 %q 不是 IP", host)
	}
	for _, a := range approved {
		if a.Equal(ip) {
			return nil
		}
	}
	return fmt.Errorf("fetch: 拨号 IP %s 不在 checkSSRF 批准集（DNS rebinding 防护）", ip)
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
