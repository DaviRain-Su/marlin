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
	// Provider selects the model gateway: deepseek (OpenAI wire) | kimi
	// (Anthropic wire, Bearer auth). echo needs no credential.
	Provider string
	// WireProtocol overrides the wire: anthropic | openai |
	// openai-responses (default per provider).
	Protocol string
	// Auth overrides the credential header: bearer | provider-default
	// (kimi defaults to bearer; api.anthropic.com to x-api-key).
	Auth string
	// APIKey is the provider credential; it overrides the environment.
	APIKey string
	// BaseURL overrides the provider's default endpoint (a base, not the
	// full request path — the host expands /v1/messages etc.).
	BaseURL string
	// Model overrides the provider's default model name.
	Model string
	// SessionID pins the Session binding ("" lets the host choose).
	SessionID string
	// StoragePath is the libsql session journal (host-side journal).
	StoragePath string
	// ToolsWorkspace enables the host tool surface: a run_command shell
	// tool rooted at this directory. Empty disables tools (default).
	ToolsWorkspace string
	// ToolsAllow is a comma-separated tool-name allowlist ("*" = all).
	ToolsAllow string
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
		case "protocol":
			c.Protocol = value
		case "auth":
			c.Auth = value
		case "api_key":
			c.APIKey = value
		case "base_url":
			c.BaseURL = value
		case "model":
			c.Model = value
		case "session_id":
			c.SessionID = value
		case "storage_path":
			c.StoragePath = value
		case "tools_workspace":
			c.ToolsWorkspace = value
		case "tools_allow":
			c.ToolsAllow = value
		default:
			// Unknown keys stay ignored: the config grows before this
			// reader does.
		}
	}
	return c, nil
}

// SaveConfig writes c as a flat TOML file (0600), atomically via temp file
// + rename. Empty fields are omitted so defaults and env fallbacks apply.
func SaveConfig(path string, c HostConfig) error {
	var b strings.Builder
	b.WriteString("# Written by the Marlin settings UI. Keys read by the packaged host.\n")
	field := func(key, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%s = %q\n", key, value)
		}
	}
	field("provider", c.Provider)
	field("protocol", c.Protocol)
	field("auth", c.Auth)
	field("api_key", c.APIKey)
	field("base_url", c.BaseURL)
	field("model", c.Model)
	field("session_id", c.SessionID)
	field("storage_path", c.StoragePath)
	field("tools_workspace", c.ToolsWorkspace)
	field("tools_allow", c.ToolsAllow)

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("sidecar: config dir: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("sidecar: write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
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
