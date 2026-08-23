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
	"maps"
	"net/http"

	"github.com/justphantom/miniagent/config"
)

// maskedSecret is the placeholder the GET response uses for non-empty secret fields
// (provider.key / web.key). A PUT carrying this exact value means "leave unchanged";
// anything else replaces the stored secret.
const maskedSecret = "********"

// configGetResponse is the GET /api/config body. Config carries the full config with
// secrets masked; writable is false when the server has no config file to write back
// to (e.g. tests); path is the config file path when writable.
type configGetResponse struct {
	Writable bool           `json:"writable"`
	Path     string         `json:"path,omitempty"`
	Config   *config.Config `json:"config"`
}

func (s *webServer) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	if cfg == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config not loaded"})
		return
	}
	// Mask secrets for the client: the UI must not render the plaintext key back.
	writeJSON(w, http.StatusOK, configGetResponse{
		Writable: s.cfgPath != "",
		Path:     s.cfgPath,
		Config:   maskedConfig(cfg),
	})
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
	applyMaskedSecrets(s.cfg, &incoming)
	// Validate before writing: a bad edit must not leave the file in an invalid state.
	if err := config.ValidateConfig(&incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := config.SaveConfig(s.cfgPath, &incoming); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save config: " + err.Error()})
		return
	}
	needRestart := !configEqual(s.cfg, &incoming)
	s.cfg = &incoming
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
func applyMaskedSecrets(cur, dst *config.Config) {
	if cur == nil || dst == nil {
		return
	}
	if dst.Web.Key == maskedSecret {
		dst.Web.Key = cur.Web.Key
	}
	for i := range dst.Providers {
		if dst.Providers[i].Key == maskedSecret {
			for _, c := range cur.Providers {
				if c.Name == dst.Providers[i].Name {
					dst.Providers[i].Key = c.Key
					break
				}
			}
		}
	}
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
