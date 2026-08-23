package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/justphantom/miniagent/miniagent"
)

// SaveConfig writes cfg to path as JSON (0600, no-follow, strict validation).
// Runs validateConfig before writing; returns validation errors as-is.
func SaveConfig(path string, cfg *Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	_ = tmp.Close()
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", writeErr)
	}
	// Rename atomically replaces path. OpenNoFollow is not used for the temp file (same dir, not user-supplied);
	// the final rename'd path is hardened via OpenNoFollow on the LoadConfig side.
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}
	// fsync parent directory for crash durability of the rename.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	// Re-open with O_NOFOLLOW to harden: if path was replaced by a symlink between rename and here,
	// OpenNoFollow rejects it. The rename already succeeded, so a failure here means the file exists
	// but is a symlink target — treat as an error.
	f, err := miniagent.OpenNoFollow(path, os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("path %q is a symlink: %w", path, err)
	}
	_ = f.Close()
	return nil
}

// ValidateConfig is the exported validation entry point (the web layer uses it to vet an
// edited config before writing it back). Identical to the internal validateConfig used by LoadConfig.
func ValidateConfig(cfg *Config) error { return validateConfig(cfg) }
