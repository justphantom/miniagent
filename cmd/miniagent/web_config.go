package main

// web_config.go implements the WebUI configuration management page backend:
// GET /api/config reads the effective config (secrets masked, write-back flag) and
// PUT /api/config validates + writes the edited config back to the config file.
//
// The frontend owns the field schema/rendering; this handler only serves the raw
// config object, guards secret round-trips and preserves the no-hot-reload contract
// (any change to runtime-affecting fields needs a service restart to take effect).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"

	"github.com/justphantom/miniagent/config"
)

// maskedSecret is the placeholder the GET response uses for non-empty secret fields
// (provider.key / web.key). A PUT carrying this exact value means "leave unchanged";
// anything else replaces the stored secret.
const maskedSecret = "********"

// configGetResponse is the GET /api/config body. Config is the edit basis: the FILE
// config when readable (the file is what PUT writes, so edits must start from it),
// falling back to the running config. Diverged/Diff describe file-vs-running drift so
// the UI can show that a restart is needed for the saved values to take effect;
// FileError is set when the file exists but cannot be parsed (external corruption).
type configGetResponse struct {
	Writable  bool           `json:"writable"`
	Path      string         `json:"path,omitempty"`
	Diverged  bool           `json:"diverged"`
	Diff      []string       `json:"diff,omitempty"` // dotted field paths only; values are never sent
	FileError string         `json:"file_error,omitempty"`
	Config    *config.Config `json:"config"`
}

func (s *webServer) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	if cfg == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config not loaded"})
		return
	}
	resp := configGetResponse{Writable: s.cfgPath != "", Path: s.cfgPath, Config: maskedConfig(cfg)}
	if s.cfgPath != "" {
		if file, err := config.LoadConfig(s.cfgPath); err != nil {
			resp.FileError = err.Error() // fall back to the running config as edit basis; saving overwrites the bad file
		} else {
			resp.Config = maskedConfig(file)
			resp.Diff = configDiffPaths(cfg, file)
			resp.Diverged = len(resp.Diff) > 0
		}
	}
	// Mask secrets for the client: the UI must not render the plaintext key back.
	writeJSON(w, http.StatusOK, resp)
}

// configDiffPaths walks two configs marshalled to generic JSON and returns the dotted
// paths where they differ (e.g. "run.max_tokens", "providers.0.key"). Paths only —
// values are deliberately excluded so a diverged secret never leaves the server.
// Both sides go through the same marshal, so secret fields compare mask-to-mask.
func configDiffPaths(running, file *config.Config) []string {
	var a, b any
	if ab, err := json.Marshal(running); err == nil {
		_ = json.Unmarshal(ab, &a)
	}
	if bb, err := json.Marshal(file); err == nil {
		_ = json.Unmarshal(bb, &b)
	}
	if a == nil || b == nil {
		return nil // marshal failure: report no diff rather than a misleading list
	}
	var out []string
	var walk func(x, y any, path string)
	walk = func(x, y any, path string) {
		mx, okX := x.(map[string]any)
		my, okY := y.(map[string]any)
		if okX && okY {
			keys := make([]string, 0, len(mx)+len(my))
			for k := range mx {
				keys = append(keys, k)
			}
			for k := range my {
				if _, dup := mx[k]; !dup {
					keys = append(keys, k)
				}
			}
			slices.Sort(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(mx[k], my[k], p)
			}
			return
		}
		if sx, ok := x.([]any); ok {
			if sy, ok2 := y.([]any); ok2 {
				n := max(len(sx), len(sy))
				for i := range n {
					p := strconv.Itoa(i)
					if path != "" {
						p = path + "." + p
					}
					var xi, yi any
					if i < len(sx) {
						xi = sx[i]
					}
					if i < len(sy) {
						yi = sy[i]
					}
					walk(xi, yi, p)
				}
				return
			}
		}
		if !reflect.DeepEqual(x, y) {
			out = append(out, path)
		}
	}
	walk(a, b, "")
	return out
}

// handleReload validates that the config file loads and no turn is in flight, then
// signals runServe's restart loop. The handler answers before the socket drops — the
// client polls /api/whoami to detect when the new generation is up.
func (s *webServer) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.cfgPath == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no config file backing this server"})
		return
	}
	if n := s.turns.runningCount(); n > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("%d turn(s) in flight; stop them before reloading", n)})
		return
	}
	if _, err := config.LoadConfig(s.cfgPath); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config file does not load: " + err.Error()})
		return
	}
	select {
	case s.reloadCh <- struct{}{}:
		writeJSON(w, http.StatusOK, map[string]string{"message": "reloading"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reload already pending"})
	}
}

// configPutResponse is the PUT /api/config body.
type configPutResponse struct {
	OK          bool           `json:"ok"`
	NeedRestart bool           `json:"need_restart"` // true = the saved config differs from the running one → restart required
	Message     string         `json:"message"`
	Config      *config.Config `json:"config,omitempty"` // saved config (secrets masked), for the UI to refill the form
}

// handleConfigPut validates and persists an edited config. Secrets that came back as
// the masked placeholder are preserved (the client cannot read them, so it must be able
// to send them back unchanged); any other value replaces the stored secret.
func (s *webServer) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	if s.cfgPath == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "config file write-back is not available"})
		return
	}
	if r.Body == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config body is required"})
		return
	}
	var incoming config.Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBodyBytes))
	if err := dec.Decode(&incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid config JSON: " + err.Error()})
		return
	}
	// Preserve stored secrets when the client sent back the masked placeholder.
	if err := applyMaskedSecrets(s.cfg, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Validate before writing: a bad edit must not leave the file in an invalid state.
	if err := config.ValidateConfig(&incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := config.SaveConfig(s.cfgPath, &incoming); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save config: " + err.Error()})
		return
	}
	// The running config is NOT swapped: other handlers read s.cfg concurrently without a lock
	// (a swap would be a data race), and selectively hot-reloading some fields while others need
	// a restart is incoherent. The file is the source of truth; every runtime-affecting change
	// takes effect after restart. needRestart therefore means "the saved file differs from the
	// running config".
	needRestart := !configEqual(s.cfg, &incoming)
	msg := "配置已保存"
	if needRestart {
		msg += "；服务运行参数已变更，需重启 miniagent 后生效"
	} else {
		msg += "；当前运行配置无需重启"
	}
	writeJSON(w, http.StatusOK, configPutResponse{
		OK:          true,
		NeedRestart: needRestart,
		Message:     msg,
		Config:      maskedConfig(&incoming), // refill the form with the authoritative saved value
	})
}

// maskedConfig returns a shallow copy of cfg with all secret fields masked, safe for the
// client to render back. The provider slice and headers maps are deep-copied so mutating
// the returned value cannot alias the server's in-memory config.
func maskedConfig(cfg *config.Config) *config.Config {
	m := *cfg
	m.Providers = make([]config.ProviderConfig, len(cfg.Providers))
	copy(m.Providers, cfg.Providers)
	for i := range m.Providers {
		if m.Providers[i].Key != "" {
			m.Providers[i].Key = maskedSecret
		}
		if m.Providers[i].Headers != nil {
			m.Providers[i].Headers = maps.Clone(m.Providers[i].Headers)
		}
	}
	m.Web.Key = maskIfSet(cfg.Web.Key)
	return &m
}

// maskIfSet returns the masked placeholder for a non-empty secret, else "".
func maskIfSet(v string) string {
	if v == "" {
		return ""
	}
	return maskedSecret
}

// applyMaskedSecrets copies stored secrets into dst where dst carried the placeholder.
// Provider.key and web.key are the only secrets; other fields pass through verbatim.
// Renaming a provider breaks the name-keyed lookup — an error (instead of silently persisting
// the literal mask as the key, which would break that provider's auth) makes the client
// re-edit: rename and key change cannot ride the same save.
func applyMaskedSecrets(cur, dst *config.Config) error {
	if cur == nil || dst == nil {
		return nil
	}
	if dst.Web.Key == maskedSecret {
		dst.Web.Key = cur.Web.Key
	}
	for i := range dst.Providers {
		if dst.Providers[i].Key != maskedSecret {
			continue
		}
		var found bool
		for _, c := range cur.Providers {
			if c.Name == dst.Providers[i].Name {
				dst.Providers[i].Key = c.Key
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("provider %q 带着掩码 key 但当前配置无同名 provider：重命名与换 key 不能同一次保存完成，请先重命名并填入新 key", dst.Providers[i].Name)
		}
	}
	return nil
}

// configEqual reports whether two configs marshal to the same JSON — the restart-need
// signal. Field order is stable (struct order), so a canonical marshal compare is exact.
func configEqual(a, b *config.Config) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// maxConfigBodyBytes caps the PUT body: the config file itself is capped at 4 MiB on load,
// so the same bound applies on write.
const maxConfigBodyBytes = 4 << 20
