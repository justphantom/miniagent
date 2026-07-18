package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxWebFetchChars = 20000
const webfetchTimeout = 30 * time.Second
const maxWebFetchRedirects = 3

// WebFetchTool returns a webfetch tool.
func WebFetchTool(httpClient *http.Client) Tool {
	return Tool{
		Name:        "webfetch",
		Description: "抓取一个 http(s) 网页并返回其纯文本内容（已去掉 script/style/HTML 标签，最长 " + fmt.Sprintf("%d", maxWebFetchChars) + " 字符）。",
		Parameters: object(map[string]any{
			"url": map[string]any{"type": "string", "description": "要抓取的完整 http(s) URL"},
		}, "url"),
		Call: func(ctx context.Context, args string) ToolResult {
			var a struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
			}
			if strings.TrimSpace(a.URL) == "" {
				return ToolResult{IsError: true, Output: "参数缺失：url"}
			}
			if !isHTTPURL(a.URL) {
				return ToolResult{IsError: true, Output: fmt.Sprintf("仅支持 http/https URL，收到 %q", a.URL)}
			}
			client := webfetchClient(httpClient)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
			if err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("构造请求失败：%v", err)}
			}
			req.Header.Set("User-Agent", "miniagent/webfetch")
			resp, err := client.Do(req)
			if err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("抓取失败：%v", err)}
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
			if err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("读取响应失败：%v", err)}
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return ToolResult{IsError: true, Output: fmt.Sprintf("%s 返回 %d：%s", a.URL, resp.StatusCode, truncate(string(body), 200, "…"))}
			}
			ctype := resp.Header.Get("Content-Type")
			if strings.Contains(ctype, "text/html") {
				return ToolResult{Output: truncate(htmlToText(body), maxWebFetchChars, "…")}
			}
			return ToolResult{Output: truncate(string(body), maxWebFetchChars, "…")}
		},
	}
}

func isHTTPURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("webfetch: invalid address %q", addr)
	}
	if host == "" {
		return nil, fmt.Errorf("webfetch: empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("webfetch: private address refused: %s", host)
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return nil, fmt.Errorf("webfetch: localhost refused")
	}
	addrs, err := (&net.Resolver{}).LookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("webfetch: resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("webfetch: no addresses for %q", host)
	}
	var selected string
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !isPublicIP(ip) {
			return nil, fmt.Errorf("webfetch: private address refused for %q", host)
		}
		if selected == "" {
			selected = a
		}
	}
	return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(selected, port))
}

func isPublicIP(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsPrivate() && !ip.IsMulticast() && !ip.IsUnspecified()
}

func webfetchClient(src *http.Client) *http.Client {
	if src == nil {
		return webfetchDefaultClient()
	}
	c := *src
	switch t := c.Transport.(type) {
	case nil:
		c.Transport = &http.Transport{DialContext: safeDialContext}
	case *http.Transport:
		tr := t.Clone()
		tr.DialContext = safeDialContext
		c.Transport = tr
	default:
		// 自定义 RoundTripper 无法注入 DialContext，保留原行为。
	}
	return &c
}

func webfetchDefaultClient() *http.Client {
	return &http.Client{
		Timeout: webfetchTimeout,
		Transport: &http.Transport{
			DialContext: safeDialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxWebFetchRedirects {
				return fmt.Errorf("webfetch: stopped after %d redirects", maxWebFetchRedirects)
			}
			if !isHTTPURL(req.URL.String()) {
				return fmt.Errorf("webfetch: redirect to non-http(s) URL refused: %s", req.URL.Scheme)
			}
			return nil
		},
	}
}

var skipTags = map[string]bool{
	"script": true, "style": true, "title": true, "noscript": true,
}

var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "dd": true, "div": true, "dl": true, "dt": true,
	"fieldset": true, "figcaption": true, "figure": true, "footer": true,
	"form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "header": true, "hr": true, "li": true, "main": true,
	"nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"table": true, "tbody": true, "td": true, "tfoot": true, "th": true,
	"thead": true, "tr": true, "ul": true,
}

// htmlToText extracts visible text from HTML using a tiny tokenizer.
// It skips script/style/title/noscript, inserts newlines for block tags,
// and strips remaining tags.
func htmlToText(body []byte) string {
	var out strings.Builder
	var skipDepth int
	var lastTag string
	var inTag bool
	var tagName strings.Builder
	var text strings.Builder
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		s := strings.TrimSpace(text.String())
		text.Reset()
		if s == "" {
			return
		}
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") && !strings.HasSuffix(out.String(), " ") {
			out.WriteByte(' ')
		}
		out.WriteString(s)
	}
	for i := 0; i < len(body); i++ {
		b := body[i]
		if inTag {
			if b == '>' {
				inTag = false
				raw := tagName.String()
				tagName.Reset()
				closing := false
				if strings.HasPrefix(raw, "/") {
					closing = true
					raw = raw[1:]
				}
				name, _, _ := strings.Cut(raw, " ")
				name = strings.ToLower(name)
				if closing {
					if skipDepth > 0 && (skipTags[name] || (name != "" && lastTag == name)) {
						skipDepth--
						if skipDepth == 0 {
							lastTag = ""
						}
					}
					if skipDepth == 0 && blockTags[name] {
						flushText()
						if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
							out.WriteByte('\n')
						}
					}
				} else {
					if skipTags[name] {
						flushText()
						skipDepth++
						lastTag = name
					} else if skipDepth == 0 && blockTags[name] {
						flushText()
						if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
							out.WriteByte('\n')
						}
					}
				}
				continue
			}
			if tagName.Len() == 0 && b == '/' {
				tagName.WriteByte('/')
			} else if b != ' ' && b != '\t' && b != '\n' && b != '\r' && b != '/' {
				tagName.WriteByte(b)
			} else if tagName.Len() > 0 && b == ' ' {
				tagName.WriteByte(' ')
			}
			continue
		}
		if b == '<' {
			flushText()
			inTag = true
			continue
		}
		if skipDepth == 0 {
			text.WriteByte(b)
		}
	}
	flushText()
	return strings.TrimSpace(out.String())
}
