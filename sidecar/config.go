package sidecar

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HostConfig is the app-sidecar configuration (design doc §5):
// model endpoint + provider credentials, read by the host at startup.
// v1 uses a flat TOML subset: `key = "value"` lines and # comments.
type HostConfig struct {
	// Provider selects the model gateway: deepseek | kimi (echo needs none).
	Provider string
	// APIKey is the provider credential; it overrides the environment.
	APIKey string
	// BaseURL overrides the provider's default endpoint.
	BaseURL string
	// Model overrides the provider's default model name.
	Model string
	// SessionID pins the Session binding ("" lets the host choose).
	SessionID string
}

// DefaultConfigPath is the user-level config location:
// <UserConfigDir>/marlin-desktop/app-sidecar.toml.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "marlin-desktop", "app-sidecar.toml"), nil
}

// LoadConfig reads path; a missing file returns the zero config without
// error (absence is normal — the app falls back to env credentials).
// Malformed values are an error: a half-written credential must not silently
// select the echo gateway.
func LoadConfig(path string) (HostConfig, error) {
	var c HostConfig
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("sidecar: read config: %w", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return c, fmt.Errorf("sidecar: config %s line %d: expected key = \"value\"", filepath.Base(path), i+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		switch key {
		case "provider":
			c.Provider = value
		case "api_key":
			c.APIKey = value
		case "base_url":
			c.BaseURL = value
		case "model":
			c.Model = value
		case "session_id":
			c.SessionID = value
		default:
			// Unknown keys stay ignored: the config grows before this
			// reader does.
		}
	}
	return c, nil
}

// WriteConfigTemplate writes a commented template (no credentials) so a
// fresh machine has something to fill in. Never overwrite an existing file.
func WriteConfigTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	const tpl = `# Marlin model provider configuration.
# provider: kimi (Anthropic-compatible) | deepseek (OpenAI-compatible)
provider = "kimi"
# api_key overrides the KIMI_API_KEY / DEEPSEEK_API_KEY environment.
# api_key = ""
# base_url and model override the provider defaults.
# base_url = ""
# model = ""
`
	return os.WriteFile(path, []byte(tpl), 0o600)
}
