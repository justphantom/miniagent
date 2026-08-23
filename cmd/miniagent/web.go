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
	cfgPath      string // config file path for PUT /api/config write-back; "" = not backed by a file (tests) → save disabled
	engine       *turnEngine
	key          string   // effective key; "" = no auth (loopback-only, enforced at startup)
	allowedHosts []string // listen host variants the Host header may carry (DNS-rebinding defense); nil = skip (tests)
	logger       *slog.Logger
	turns        *turnRegistry   // in-flight turns/deletes: event bus + same-session serialization
	baseCtx      context.Context // server lifetime; web turns derive from it, NOT from the request (D1)
	turnSem      chan struct{}   // web.max_concurrent_turns gate; nil = unlimited
	reloadCh     chan struct{}   // POST /api/reload signals runServe's restart loop; nil (tests) = reload disabled
}

// newWebServer assembles the server state shared by runServe and tests. baseCtx owns the web
// turn contexts so a client disconnect never cancels an agent turn (D1).
func newWebServer(baseCtx context.Context, cfg *config.Config, engine *turnEngine, key string, logger *slog.Logger) *webServer {
	// reloadCh buffered=1: the handler's send never blocks even if the serve loop is
	// momentarily between generations; the loop drains it before re-entering select.
	s := &webServer{cfg: cfg, engine: engine, key: key, logger: logger, turns: newTurnRegistry(), baseCtx: baseCtx, reloadCh: make(chan struct{}, 1)}
	if n := cfg.Web.MaxConcurrentTurns; n > 0 {
		s.turnSem = make(chan struct{}, n)
	}
	return s
}

// setCfgPath records the config file path backing this server (runServe calls it with the
// resolved -config path; tests that construct a server without one leave cfgPath empty and
// PUT /api/config answers 404 — there is no file to write back to).
func (s *webServer) setCfgPath(path string) { s.cfgPath = path }

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
// runServe hosts the WebUI. POST /api/reload re-reads the config file and rebuilds the
// server in place (serve loop below): auth key, allowed_hosts, listen address and the
// concurrency gate all restart-effect. In-flight turns (which derive from ctx, not the
// http.Server) are unaffected; reload is refused while any turn is running so a session
// never straddles two engine generations.
func runServe(ctx context.Context, cfg *config.Config, cfgPath string, logger *slog.Logger) {
	for {
		last := serveOnce(ctx, cfg, cfgPath, logger)
		select {
		case <-ctx.Done():
			return
		case <-last.reloadCh:
			var err error
			cfg, err = config.LoadConfig(cfgPath)
			if err != nil {
				// The handler already validated the file loads; this is a race (the file
				// changed again between validation and reload). Keep serving the old config.
				fmt.Fprintf(os.Stderr, "miniagent: reload: %v (keeping current config)\n", err)
				continue
			}
			fmt.Fprintln(os.Stderr, "miniagent: config reloaded")
		}
	}
}

// serveOnce builds one server generation from cfg and blocks until shutdown, ctx cancel,
// or a reload request. Returns the server so runServe can read its reload channel.
func serveOnce(ctx context.Context, cfg *config.Config, cfgPath string, logger *slog.Logger) *webServer {
	addr := cfg.Web.Listen
	if addr == "" {
		addr = defaultWebListen
	}
	key := webKey(cfg)
	if key == "" && !listenHostIsLoopback(addr) {
		fmt.Fprintf(os.Stderr, "miniagent: web.listen %q is not loopback but no key is set (web.key or $%s); refusing to start\n", addr, webEnvKey)
		os.Exit(1)
	}
	engine := &turnEngine{cfg: cfg, cfgPath: cfgPath, logger: logger, buildClients: buildRuntimeClients, protectSignal: false, transports: &transportCache{}}
	s := newWebServer(ctx, cfg, engine, key, logger)
	s.setCfgPath(cfgPath)
	s.allowedHosts = hostVariants(addr, cfg.Web.AllowedHosts)

	srv := &http.Server{Addr: addr, Handler: s.mux(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: web listen %s: %v\n", addr, err)
		os.Exit(1)
	}
	if cfg.Run.ConfirmDestructive != nil && *cfg.Run.ConfirmDestructive {
		// serve mode stdin is not a TTY → destructive tools are denied-by-default with no browser
		// confirmation channel. Surface at startup so the operator understands why write/edit fail.
		fmt.Fprintln(os.Stderr, "miniagent: -serve with confirm_destructive=true — destructive tools (write/edit/dangerous shell) will be DENIED (no TTY prompt); set MINIAGENT_AUTO_APPROVE=1 to allow")
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
	case <-s.reloadCh:
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
	return s
}

// mux wires routes: static pages are public (no secrets embedded); /api/* is auth-gated
// except /api/whoami, which the login page needs to probe whether a key is required.
// The whole tree is wrapped in s.guard (Host/Origin validation — web_guard.go).
func (s *webServer) mux() http.Handler {
	mux := http.NewServeMux()
	// GET /{$} serves index.html only at the root; plain "GET /" would conflict with the
	// method-agnostic "/api/" subtree (root pattern beats it by method specificity).
	mux.HandleFunc("GET /{$}", s.serveStatic("index.html"))
	// N10: register every embedded static asset by name instead of hand-listing the 6 files in
	// two places (webstatic assets.go now owns the canonical list via embed.FS).
	for _, f := range webstatic.Names() {
		mux.HandleFunc("GET /"+f, s.serveStatic(f))
	}
	mux.HandleFunc("GET /api/whoami", s.whoami)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/config", s.handleConfigGet)
	api.HandleFunc("PUT /api/config", s.handleConfigPut)
	api.HandleFunc("POST /api/reload", s.handleReload)
	api.HandleFunc("GET /api/tree", s.handleTree)
	api.HandleFunc("GET /api/models", s.handleModels)
	api.HandleFunc("POST /api/turn", s.handleTurn)
	api.HandleFunc("GET /api/events", s.handleEvents)
	api.HandleFunc("GET /api/sessions", s.handleSessionsList)
	api.HandleFunc("GET /api/sessions/{id}", s.handleSessionReplay)
	api.HandleFunc("GET /api/sessions/{id}/live", s.handleSessionLive)
	api.HandleFunc("DELETE /api/sessions/{id}", s.handleSessionDelete)
	api.HandleFunc("POST /api/sessions/{id}/stop", s.handleSessionStop)
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

// ndjsonWriter streams NDJSON lines to the response, flushing after each so the UI renders
// incrementally. A write error (client gone) is sticky — later lines are dropped and the
// caller treats it as "stop mirroring"; the turn itself keeps running on the bus (D1).
type ndjsonWriter struct {
	w   io.Writer
	f   http.Flusher
	err error
}

func (nw *ndjsonWriter) WriteLine(line string) error {
	if nw.err != nil {
		return nw.err
	}
	if _, err := nw.w.Write([]byte(line)); err != nil {
		nw.err = err
		return err
	}
	if nw.f != nil {
		nw.f.Flush()
	}
	return nil
}
