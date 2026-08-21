package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	miniagent "github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/text"
)

// webFetchTimeout is the default overall timeout for one web fetch (connect+headers+body).
// Distinct from http_timeout (LLM API, 180s default): web pages return fast or never.
const webFetchTimeout = 30 * time.Second

// maxWebBodyBytes caps the response body read into memory (aligned with read's 1MB per-file cap).
const maxWebBodyBytes = 1 << 20

// maxWebRedirects bounds the redirect chain; each hop re-validates the SSRF guard (CheckRedirect).
const maxWebRedirects = 5

type webArgs struct {
	URL string `json:"url"`
}

// WebTool returns a GET-only web fetch tool: retrieves a URL and returns extracted text
// for the LLM context. Guardrails (best-effort, NOT a security boundary):
//   - GET only; scheme http/https
//   - SSRF: hostname must resolve to public unicast IPs (loopback/private/link-local/multicast
//     rejected), re-checked on every redirect hop (CheckRedirect), unless allowPrivate
//   - content sniffing: text/* or JSON/* (via XTD or Content-Type) required; binary rejected
//   - HTML→text: minimal tag stripping (script/style dropped, tags erased) — deliberate
//     non-dependency: x/net/html would be the project's first third-party import
//
// timeout<=0 uses webFetchTimeout; maxBytes<=0 uses maxWebBodyBytes; maxOutputChars<=0 uses maxShellOutputChars.
func WebTool(timeout time.Duration, maxBytes, maxOutputChars int, allowPrivate bool) miniagent.Tool {
	if timeout <= 0 {
		timeout = webFetchTimeout
	}
	if maxBytes <= 0 {
		maxBytes = maxWebBodyBytes
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxWebRedirects {
				return fmt.Errorf("stopped after %d redirects", maxWebRedirects)
			}
			if err := checkWebHost(req.Context(), req.URL, allowPrivate); err != nil {
				return fmt.Errorf("redirect to %s blocked: %w", req.URL.Host, err)
			}
			return nil
		},
	}
	return miniagent.Tool{
		Name:        "web",
		Description: "Fetches a web page (HTTP GET) and returns its text content for reading. Only http/https URLs; rejects loopback/private/link-local targets and redirects to them; rejects binary (non-text) content; strips HTML tags to plain text. Response body limit " + strconv.Itoa(maxBytes) + " bytes, output limit " + strconv.Itoa(maxOutputChars) + " chars, redirect limit " + strconv.Itoa(maxWebRedirects) + ".",
		Parameters: object(map[string]any{
			"url": map[string]any{"type": "string", "description": "Absolute http(s) URL to fetch"},
		}, "url"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "web", func(rctx context.Context) miniagent.ToolResult {
				return runWeb(rctx, client, args, maxBytes, maxOutputChars, allowPrivate)
			})
		},
	}
}

func runWeb(ctx context.Context, client *http.Client, args string, maxBytes, maxOutputChars int, allowPrivate bool) miniagent.ToolResult {
	var a webArgs
	if err := decodeStrict(args, &a); err != nil {
		return denyResult("argument parsing failed: %v", err)
	}
	raw := strings.TrimSpace(a.URL)
	if raw == "" {
		return denyResult("missing argument: url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return denyResult("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return denyResult("unsupported scheme %q (only http/https)", u.Scheme)
	}
	if u.Host == "" {
		return denyResult("missing host in url %q", raw)
	}
	if err := checkWebHost(ctx, u, allowPrivate); err != nil {
		return denyResult("host %q blocked: %v", u.Host, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return denyResult("build request failed: %v", err)
	}
	req.Header.Set("User-Agent", miniagent.UserAgent())
	req.Header.Set("Accept", "text/html,text/plain,application/json,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("web fetch %q failed: %v", raw, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("web fetch %q returned HTTP %d %s", raw, resp.StatusCode, http.StatusText(resp.StatusCode))}
	}
	body, truncated, err := readWebBody(resp.Body, maxBytes)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("web read %q failed: %v", raw, err)}
	}
	ctype := resp.Header.Get("Content-Type")
	body, charsetWarn := decodeWebBody(ctype, body)
	if charsetWarn != "" {
		charsetWarn = "[warning: " + charsetWarn + "]\n"
	}
	kind := webContentKind(ctype, body)
	if kind == webBinary {
		return denyResult("content is binary (content-type %q); web only supports text", ctype)
	}
	out := webToText(body, kind == webHTML)
	out = charsetWarn + text.Truncate(out, maxOutputChars, "…[web output truncated]")
	if truncated {
		out += fmt.Sprintf("\n…[body over %d bytes, truncated]", maxBytes)
	}
	return miniagent.ToolResult{Output: out}
}

// checkWebHost resolves the URL hostname and rejects non-public targets (SSRF guard).
// IP literals are checked directly; names are resolved and every resulting IP must be public
// unicast. allowPrivate (test/CI hook) skips the check.
func checkWebHost(ctx context.Context, u *url.URL, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("%s is not a public address", ip)
		}
		return nil
	}
	var resolver net.Resolver
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %q failed: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip.IP) {
			return fmt.Errorf("%s resolves to non-public %s", host, ip.IP)
		}
	}
	return nil
}

// isPublicIP reports whether ip is public unicast: not loopback, private, link-local
// (covers cloud metadata 169.254.169.254), multicast, unspecified, or interface-local multicast.
func isPublicIP(ip net.IP) bool {
	blocked := ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast()
	return !blocked
}

// readWebBody reads up to maxBytes+1 (the +1 detects truncation).
func readWebBody(r io.Reader, maxBytes int) (string, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil {
		return "", false, err
	}
	truncated := len(raw) > maxBytes
	if truncated {
		raw = raw[:maxBytes]
	}
	return string(raw), truncated, nil
}

type webKind int

const (
	webHTML webKind = iota
	webText
	webBinary
)

// webContentKind decides HTML vs text vs binary from Content-Type, falling back to a NUL-byte
// sniff on the first 512 bytes when the header is missing/uninformative.
func webContentKind(ctype string, body string) webKind {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(ctype, ";", 2)[0]))
	switch {
	case ct == "" || ct == "application/octet-stream":
		if strings.IndexByte(body[:min(len(body), 512)], 0) >= 0 {
			return webBinary
		}
		if looksLikeHTML(body) {
			return webHTML
		}
		return webText
	case ct == "text/html" || ct == "application/xhtml+xml":
		return webHTML
	case strings.HasPrefix(ct, "text/") || ct == "application/json" || ct == "application/xml" ||
		strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml"):
		return webText
	default:
		return webBinary
	}
}

// looksLikeHTML reports whether the body starts like an HTML document (after whitespace/BOM).
func looksLikeHTML(body string) bool {
	s := strings.TrimSpace(body)
	s = strings.TrimPrefix(s, "\uFEFF")
	lower := strings.ToLower(s[:min(len(s), 256)])
	return strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") ||
		strings.HasPrefix(lower, "<head") || strings.HasPrefix(lower, "<body")
}
