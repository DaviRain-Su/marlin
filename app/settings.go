package app

import (
	"fmt"
	"log/slog"

	"marlin-desktop/sidecar"
)

// SettingsSnapshot is what the settings page reads: the effective config
// with the credential masked (the page can set a key but never read one).
type SettingsSnapshot struct {
	ConfigPath     string `json:"configPath"`
	Provider       string `json:"provider"`
	Model          string `json:"model,omitempty"`
	BaseURL        string `json:"baseUrl,omitempty"`
	ToolsWorkspace string `json:"toolsWorkspace,omitempty"`
	ToolsAllow     string `json:"toolsAllow,omitempty"`
	KeyConfigured  bool   `json:"keyConfigured"`
	// HostRunning reflects whether a config change would need a restart to
	// take effect (false: it applies at the next host start anyway).
	HostRunning bool `json:"hostRunning"`
}

// SettingsInput is one save from the page. An empty APIKey keeps the
// stored credential ("" is indistinguishable from "unchanged" over the
// wire; clearing a key means switching to env credentials, done by
// editing the file).
type SettingsInput struct {
	Provider       string `json:"provider"`
	Model          string `json:"model,omitempty"`
	BaseURL        string `json:"baseUrl,omitempty"`
	APIKey         string `json:"apiKey,omitempty"`
	ToolsWorkspace string `json:"toolsWorkspace,omitempty"`
	ToolsAllow     string `json:"toolsAllow,omitempty"`
}

// SettingsService is bound to the frontend: SettingsService.state / save /
// apply. It owns the user-level app-sidecar.toml (design §5.2); the host
// picks up changes on its next start.
type SettingsService struct {
	configPath string
	chat       *ChatService
}

// NewSettingsService builds the service over the given config path
// (sidecar.DefaultConfigPath in production).
func NewSettingsService(configPath string, chat ...*ChatService) *SettingsService {
	s := &SettingsService{configPath: configPath}
	if len(chat) > 0 {
		s.chat = chat[0]
	}
	return s
}

// State reads the current config with the credential masked.
func (s *SettingsService) State() (SettingsSnapshot, error) {
	cfg, err := sidecar.LoadConfig(s.configPath)
	if err != nil {
		return SettingsSnapshot{}, fmt.Errorf("settings: read config: %w", err)
	}
	return SettingsSnapshot{
		ConfigPath:     s.configPath,
		Provider:       cfg.Provider,
		Model:          cfg.Model,
		BaseURL:        cfg.BaseURL,
		ToolsWorkspace: cfg.ToolsWorkspace,
		ToolsAllow:     cfg.ToolsAllow,
		KeyConfigured:  cfg.APIKey != "",
		HostRunning:    hostRunning(),
	}, nil
}

// Save merges the input into the config file. Provider is validated: the
// host only routes kimi and deepseek (echo needs no credential).
func (s *SettingsService) Save(in SettingsInput) error {
	if s.chat != nil {
		if !s.chat.operation.TryLock() {
			return fmt.Errorf("a session operation is running")
		}
		defer s.chat.operation.Unlock()
	}
	switch in.Provider {
	case "kimi", "deepseek", "":
	default:
		return fmt.Errorf("settings: unsupported provider %q", in.Provider)
	}
	cfg, err := sidecar.LoadConfig(s.configPath)
	if err != nil {
		return fmt.Errorf("settings: read config: %w", err)
	}
	if in.Provider != "" {
		previousProvider := cfg.Provider
		if previousProvider == "" {
			previousProvider = "kimi"
		}
		if previousProvider != in.Provider {
			cfg.APIKey, cfg.Protocol, cfg.Auth = "", "", ""
		}
		cfg.Provider = in.Provider
	}
	cfg.Model = in.Model
	cfg.BaseURL = in.BaseURL
	if in.APIKey != "" {
		cfg.APIKey = in.APIKey
	}
	// Tools are opt-in and workspace-rooted; an empty workspace disables.
	cfg.ToolsWorkspace = in.ToolsWorkspace
	cfg.ToolsAllow = in.ToolsAllow
	if err := sidecar.SaveConfig(s.configPath, cfg); err != nil {
		return fmt.Errorf("settings: save config: %w", err)
	}
	slog.Info("settings saved", "path", s.configPath, "provider", cfg.Provider)
	return nil
}

// Apply restarts the host so the saved config takes effect immediately.
func (s *SettingsService) Apply() error {
	if s.chat != nil {
		if !s.chat.operation.TryLock() {
			return fmt.Errorf("a session operation is running")
		}
		defer s.chat.operation.Unlock()
		if s.chat.busy() {
			return fmt.Errorf("wait for the current turn before restarting")
		}
		s.chat.setManager(nil)
	}
	return RestartHost()
}
