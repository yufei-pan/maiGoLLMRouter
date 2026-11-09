package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// Reload reads the config file at path. When the file leaves client_keys
// empty, the key prev generated stays the only key, so a reload doesn't change
// it. Keys that came from the file are gone once the file no longer lists
// them: with no generated key in prev, a new one is generated.
func Reload(path string, prev *Config) (*Config, error) {
	var raw rawConfig
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	generated := ""
	if prev != nil {
		generated = prev.Server.GeneratedClientKey
	}
	return build(raw, generated)
}
