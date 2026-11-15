package sidecar

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// HostConfig is the app-sidecar configuration (design doc §5):
// model endpoint + provider credentials, read by the host at startup.
// Unknown host keys are preserved when the app changes known settings.
type HostConfig struct {
	// Provider selects the model gateway: deepseek (OpenAI wire) | kimi
	// (Anthropic wire, Bearer auth). echo needs no credential.
	Provider string `toml:"provider,omitempty"`
	// WireProtocol overrides the wire: anthropic | openai |
	// openai-responses (default per provider).
	Protocol string `toml:"protocol,omitempty"`
	// Auth overrides the credential header: bearer | provider-default
	// (kimi defaults to bearer; api.anthropic.com to x-api-key).
	Auth string `toml:"auth,omitempty"`
	// APIKey is the provider credential; it overrides the environment.
	APIKey string `toml:"api_key,omitempty"`
	// BaseURL overrides the provider's default endpoint (a base, not the
	// full request path — the host expands /v1/messages etc.).
	BaseURL string `toml:"base_url,omitempty"`
	// Model overrides the provider's default model name.
	Model string `toml:"model,omitempty"`
	// SessionID pins the Session binding ("" lets the host choose).
	SessionID string `toml:"session_id,omitempty"`
	// StoragePath is the libsql session journal (host-side journal).
	StoragePath string `toml:"storage_path,omitempty"`
	// ToolsWorkspace enables the host tool surface: a run_command shell
	// tool rooted at this directory. Empty disables tools (default).
	ToolsWorkspace string `toml:"tools_workspace,omitempty"`
	// ToolsAllow is a comma-separated tool-name allowlist ("*" = all).
	ToolsAllow string `toml:"tools_allow,omitempty"`
}

// DefaultConfigPath is the user-level config location:
// <UserConfigDir>/marlin-desktop/app-sidecar.toml.
func DefaultConfigPath() (string, error) {
	if dir := os.Getenv("MARLIN_DATA_DIR"); dir != "" {
		return filepath.Join(dir, "app-sidecar.toml"), nil
	}
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
	if err := toml.Unmarshal(data, &c); err != nil {
		return HostConfig{}, fmt.Errorf("sidecar: malformed config %s", filepath.Base(path))
	}
	return c, nil
}

// SaveConfig writes c as TOML (0600), preserving unknown host settings.
// Empty known fields are removed so defaults and env fallbacks apply.
func SaveConfig(path string, c HostConfig) error {
	values := map[string]any{}
	previous, err := os.ReadFile(path)
	if err == nil {
		if err := toml.Unmarshal(previous, &values); err != nil {
			return fmt.Errorf("sidecar: cannot overwrite malformed config")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("sidecar: read config: %w", err)
	}
	for _, key := range []string{"provider", "protocol", "auth", "api_key", "base_url", "model", "session_id", "storage_path", "tools_workspace", "tools_allow"} {
		delete(values, key)
	}
	known, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := toml.Unmarshal(known, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		values[key] = value
	}
	data, err := toml.Marshal(values)
	if err != nil {
		return fmt.Errorf("sidecar: encode config")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("sidecar: config dir: %w", err)
		}
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("sidecar: rename config: %w", err)
	}
	return nil
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
	const tpl = `# Marlin model provider configuration (read by the packaged host).
# provider: kimi (Anthropic wire, Bearer) | deepseek (OpenAI wire)
provider = "kimi"
# api_key overrides the KIMI_API_KEY / DEEPSEEK_API_KEY environment.
# api_key = ""
# base_url and model override the provider defaults.
# base_url = ""
# model = ""
# protocol: anthropic | openai | openai-responses (default per provider)
# auth: bearer | provider-default (kimi defaults to bearer)
# storage_path: libsql session journal (default under the config dir)
# tools_workspace: enable the run_command shell tool, rooted at this path
# tools_allow: comma-separated tool names, "*" = all
`
	return os.WriteFile(path, []byte(tpl), 0o600)
}
