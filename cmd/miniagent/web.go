package main

// web.go assembles the -serve HTTP server: embedded single-page UI (webstatic), x-api-key
// auth gate, and the JSON/NDJSON API. The auth key is the only boundary over the zero-safety
// agent (L0 #13) — non-loopback listen without a key is rejected at startup, never at runtime.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/justphantom/miniagent/cmd/miniagent/webstatic"
	"github.com/justphantom/miniagent/config"
)

// defaultWebListen is the fallback listen address when config web.listen is unset.
const defaultWebListen = "127.0.0.1:8787"

// webEnvKey mirrors the provider key pattern: env fallback for secret material.
const webEnvKey = "MINIAGENT_WEB_KEY"

type webServer struct {
	cfg          *config.Config
	engine       *turnEngine
	key          string   // effective key; "" = no auth (loopback-only, enforced at startup)
	allowedHosts []string // listen host variants the Host header may carry (DNS-rebinding defense); nil = skip (tests)
	logger       *slog.Logger
	locks        sync.Map // session id → *sync.Mutex: serialize turns on the same session file
}

// webKey resolves the effective WebUI key: config web.key > $MINIAGENT_WEB_KEY.
func webKey(cfg *config.Config) string {
	if cfg.Web.Key != "" {
		return cfg.Web.Key
	}
	return os.Getenv(webEnvKey)
}

// listenHostIsLoopback reports whether the listen host binds loopback only.
// An empty host or wildcard (0.0.0.0/::) binds every interface → not loopback.
func listenHostIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// runServe starts the WebUI server. Startup-fatal conditions (remote listen without a key,
// listen failure) print to stderr and exit 1, mirroring the CLI failure style.
func runServe(ctx context.Context, cfg *config.Config, cfgPath string, logger *slog.Logger) {
	addr := cfg.Web.Listen
	if addr == "" {
		addr = defaultWebListen
	}
	key := webKey(cfg)
	if key == "" && !listenHostIsLoopback(addr) {
		fmt.Fprintf(os.Stderr, "miniagent: web.listen %q is not loopback but no key is set (web.key or $%s); refusing to start\n", addr, webEnvKey)
		os.Exit(1)
	}
	engine := &turnEngine{cfg: cfg, cfgPath: cfgPath, logger: logger, buildClients: buildRuntimeClients, protectSignal: false}
	s := &webServer{cfg: cfg, engine: engine, key: key, allowedHosts: hostVariants(addr), logger: logger}

	srv := &http.Server{Addr: addr, Handler: s.mux(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: web listen %s: %v\n", addr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "miniagent: web ui listening on %s (auth %s)\n", addr, map[bool]string{true: "on", false: "off"}[key != ""])

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			srv.Close()
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "miniagent: web serve: %v\n", err)
			os.Exit(1)
		}
	}
}

// mux wires routes: static pages are public (no secrets embedded); /api/* is auth-gated
// except /api/whoami, which the login page needs to probe whether a key is required.
// The whole tree is wrapped in s.guard (Host/Origin validation — web_guard.go).
func (s *webServer) mux() http.Handler {
	mux := http.NewServeMux()
	// "GET /{$}" matches only the root path — plain "GET /" would conflict with the
	// method-agnostic "/api/" subtree (root pattern beats it by method specificity).
	mux.HandleFunc("GET /{$}", s.serveStatic("index.html"))
	for _, f := range []string{"index.html", "app.js", "app.css", "store.js", "events.js", "md.js"} {
		mux.HandleFunc("GET /"+f, s.serveStatic(f))
	}
	mux.HandleFunc("GET /api/whoami", s.whoami)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/models", s.handleModels)
	api.HandleFunc("POST /api/turn", s.handleTurn)
	api.HandleFunc("GET /api/sessions", s.handleSessionsList)
	api.HandleFunc("GET /api/sessions/{id}", s.handleSessionReplay)
	api.HandleFunc("DELETE /api/sessions/{id}", s.handleSessionDelete)
	mux.Handle("/api/", s.requireAuth(api))
	return s.guard(mux)
}

// requireAuth rejects requests whose x-api-key does not constant-time-match the effective key.
// No auth when the key is empty (loopback-only mode, enforced at startup).
func (s *webServer) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.key == "" {
			next.ServeHTTP(w, r)
			return
		}
		token := r.Header.Get("X-Api-Key")
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.key)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *webServer) whoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"auth_required": s.key != "", "version": version})
}

func (s *webServer) serveStatic(name string) http.HandlerFunc {
	ctype := map[string]string{
		".html": "text/html; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
	}[filepath.Ext(name)]
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := webstatic.Read(name)
		if err != nil {
			http.Error(w, "static asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	}
}

// handleModels serves the provider/model pairs for the UI dropdown (same data source as
// -list-models; static config models when no models_url).
func (s *webServer) handleModels(w http.ResponseWriter, r *http.Request) {
	timeout, err := httpTimeoutFromConfig(s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	models, listErr := listAllModels(r.Context(), s.cfg.Providers, timeout, s.logger)
	if listErr != nil && len(models) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": listErr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ndjsonWriter streams the CLI-identical NDJSON contract over HTTP: each Write is one event
// line, flushed immediately so the UI renders incrementally. n counts bytes written so the
// handler can decide between a JSON error response (nothing streamed yet) and just returning.
type ndjsonWriter struct {
	w   io.Writer
	f   http.Flusher
	n   int64
	err error
}

func (nw *ndjsonWriter) Write(p []byte) (int, error) {
	if nw.err != nil {
		return 0, nw.err
	}
	n, err := nw.w.Write(p)
	nw.n += int64(n)
	if err != nil {
		nw.err = err
		return n, err
	}
	if nw.f != nil {
		nw.f.Flush()
	}
	return n, nil
}
