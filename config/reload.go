package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// Reload reads the config file at path. When the file leaves client_keys empty,
// keys from prev are kept so a generated startup key is not replaced on reload.
func Reload(path string, prev *Config) (*Config, error) {
	var raw rawConfig
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if len(raw.Server.ClientKeys) == 0 && prev != nil && len(prev.Server.ClientKeys) > 0 {
		raw.Server.ClientKeys = append([]string(nil), prev.Server.ClientKeys...)
	}
	cfg, err := build(raw)
	if err != nil {
		return nil, err
	}
	if len(raw.Server.ClientKeys) == 0 && prev != nil && len(prev.Server.ClientKeys) > 0 {
		cfg.Server.GeneratedClientKey = ""
	}
	return cfg, nil
}
