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
func (s *webServer) hostAllowed(host string) bool {
	if s.allowedHosts == nil {
		return true
	}
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
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
// For 0.0.0.0 / [::] (wildcard), the machine's hostname is added so it works out of the box
// (a wildcard bind is reachable via any name that resolves to a local interface).
func hostVariants(addr string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return []string{addr}
	}
	variants := []string{host}
	if port != "" {
		variants = append(variants, addr)
	}
	if isLoopbackHost(host) {
		for _, alt := range []string{"localhost", "127.0.0.1", "[::1]", "::1"} {
			if !slices.ContainsFunc(variants, func(v string) bool { return strings.EqualFold(v, alt) }) {
				variants = append(variants, alt)
			}
		}
	}
	if host == "0.0.0.0" || host == "[::]" || host == "::" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			if !slices.ContainsFunc(variants, func(v string) bool { return strings.EqualFold(v, hn) }) {
				variants = append(variants, hn)
			}
		}
	}
	return variants
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
