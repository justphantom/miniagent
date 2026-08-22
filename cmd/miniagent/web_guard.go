package main

// web_guard.go is the request-origin defense for the -serve API: DNS-rebinding Host
// validation plus CSRF rejection of cross-site browser requests. The x-api-key gate is
// meaningless to a browser attack when auth is off (loopback mode), because the attacker's
// page rides the victim's own loopback origin — these checks are what actually stop it.

import (
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
)

// guard wraps the whole /api/ subtree (and the static shell): every request must name an
// expected Host in the Host header (DNS rebinding sends attacker.com there) and browsers
// must not flag the request as cross-site (Sec-Fetch-Site / Origin).
func (s *webServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "host not allowed"})
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request rejected"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether the request Host matches an address the server actually
// listens on. Fail-closed once allowedHosts is set (startup); nil (unit tests) skips.
// IPv6 hosts are compared unbracketed (SplitHostPort strips the brackets).
func (s *webServer) hostAllowed(host string) bool {
	if s.allowedHosts == nil {
		return true
	}
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
	h = strings.Trim(h, "[]")
	for _, want := range s.allowedHosts {
		if strings.EqualFold(h, want) {
			return true
		}
	}
	return false
}

// hostVariants expands the listen address into every Host-header value a legitimate request
// can carry: the literal host plus the port-less form, and for loopback the alternate
// spellings (127.0.0.1 ↔ localhost — both resolve there; a client may type either).
// For 0.0.0.0 / [::] (wildcard), the machine's hostname and interface addresses are added:
// a wildcard bind is reachable via any of them and the browser sends whichever it used.
// extra appends config web.allowed_hosts (reverse proxy / custom domain deployments).
func hostVariants(addr string, extra []string) []string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return []string{addr}
	}
	// hostAllowed strips any port before comparing, so only the bare host matters.
	variants := []string{strings.Trim(host, "[]")}
	if isLoopbackHost(host) {
		for _, alt := range []string{"localhost", "127.0.0.1", "::1"} {
			if !slices.ContainsFunc(variants, func(v string) bool { return strings.EqualFold(v, alt) }) {
				variants = append(variants, alt)
			}
		}
	}
	if host == "0.0.0.0" || host == "[::]" || host == "::" {
		// Wildcard reaches loopback too — include its spellings before the interface sweep.
		for _, alt := range []string{"localhost", "127.0.0.1", "::1"} {
			if !slices.ContainsFunc(variants, func(v string) bool { return strings.EqualFold(v, alt) }) {
				variants = append(variants, alt)
			}
		}
		if hn, err := os.Hostname(); err == nil && hn != "" {
			if !slices.ContainsFunc(variants, func(v string) bool { return strings.EqualFold(v, hn) }) {
				variants = append(variants, hn)
			}
		}
		for _, ip := range localInterfaceHosts() {
			if !slices.ContainsFunc(variants, func(v string) bool { return strings.EqualFold(v, ip) }) {
				variants = append(variants, ip)
			}
		}
	}
	for _, e := range extra {
		e = strings.Trim(strings.TrimSpace(e), "[]")
		if e != "" && !slices.ContainsFunc(variants, func(v string) bool { return strings.EqualFold(v, e) }) {
			variants = append(variants, e)
		}
	}
	return variants
}

// localInterfaceHosts lists the machine's interface addresses (bare, no zone): what a
// wildcard-bound server is reachable under from the network. Link-local addresses are
// skipped (they are never how a deployment is addressed).
func localInterfaceHosts() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.IsLinkLocalUnicast() || ipnet.IP.IsLinkLocalMulticast() {
				continue
			}
			out = append(out, ipnet.IP.String())
		}
	}
	return out
}

// isLoopbackHost reports whether the host part is any loopback spelling (127.0.0.1, ::1,
// localhost). "::1" arrives bracketed from SplitHostPort, so unbracket it first.
func isLoopbackHost(host string) bool {
	h := strings.Trim(host, "[]")
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(h, "localhost")
}

// originAllowed checks the Origin header against the request's own Host: same-origin means
// the authority part of scheme://host[:port][/path] equals what the browser is talking to.
func originAllowed(origin, host string) bool {
	u := origin
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return strings.EqualFold(u, host)
}
