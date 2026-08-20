package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// maxConfigFileBytes is the byte cap for the config file, preventing OOM from multi-GB files.
const maxConfigFileBytes = 4 << 20 // 4 MiB

// ReadFileLimited reads path with a size limit; returns an error if it exceeds maxBytes. It opens via miniagent.OpenNoFollow,
// rejecting final-component symlinks (hardened consistently with session files), to prevent the config path (which contains the API key)
// from being symlink-hijacked to a sensitive file.
func ReadFileLimited(path string, maxBytes int64) ([]byte, error) {
	f, err := miniagent.OpenNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file %q exceeds the %d-byte limit", path, maxBytes)
	}
	return data, nil
}

// LoadConfig reads path, deserializes, and validates. Whether an explicitly-passed -config not existing is a hard error is decided by the caller.
func LoadConfig(path string) (*Config, error) {
	data, err := ReadFileLimited(path, maxConfigFileBytes)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config %q is not valid JSON: %w", path, err)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("config %q: %w", path, err)
	}
	return &cfg, nil
}
