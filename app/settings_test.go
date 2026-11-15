package app

import (
	"path/filepath"
	"testing"

	"marlin-desktop/sidecar"
)

func sidecarLoadForTest(path string) (sidecar.HostConfig, error) {
	return sidecar.LoadConfig(path)
}

func TestSettingsSaveAndStateMaskKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-sidecar.toml")
	svc := NewSettingsService(path)

	// Save over a missing file: the key must land in the file, never in
	// the snapshot.
	err := svc.Save(SettingsInput{
		Provider:       "kimi",
		APIKey:         "sk-test-123",
		Model:          "k3",
		ToolsWorkspace: "/tmp/ws",
		ToolsAllow:     "run_command",
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := svc.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.Provider != "kimi" || state.Model != "k3" || !state.KeyConfigured {
		t.Fatalf("state = %+v", state)
	}

	// A save without a key keeps the stored credential (masked in state).
	err = svc.Save(SettingsInput{Provider: "kimi", Model: "k3", ToolsWorkspace: "/tmp/ws2"})
	if err != nil {
		t.Fatal(err)
	}
	state, err = svc.State()
	if err != nil {
		t.Fatal(err)
	}
	if !state.KeyConfigured {
		t.Fatal("key must survive a keyless save")
	}
	if state.ToolsWorkspace != "/tmp/ws2" {
		t.Fatalf("workspace = %q", state.ToolsWorkspace)
	}

	// The credential stays in the file for the host to read.
	cfg, err := sidecarLoadForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "sk-test-123" {
		t.Fatalf("stored key = %q", cfg.APIKey)
	}
}

func TestSettingsRejectsUnknownProvider(t *testing.T) {
	svc := NewSettingsService(filepath.Join(t.TempDir(), "app-sidecar.toml"))
	if err := svc.Save(SettingsInput{Provider: "evil", APIKey: "k"}); err == nil {
		t.Fatal("unknown provider must be rejected")
	}
	// Empty provider is allowed (keeps the stored one).
	if err := svc.Save(SettingsInput{Model: "m"}); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsEmptyToolsWorkspaceDisablesTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-sidecar.toml")
	svc := NewSettingsService(path)
	if err := svc.Save(SettingsInput{Provider: "kimi", ToolsWorkspace: "/tmp/ws"}); err != nil {
		t.Fatal(err)
	}
	// A save with no workspace field clears it (tools off, not "keep").
	if err := svc.Save(SettingsInput{Provider: "kimi"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := sidecarLoadForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolsWorkspace != "" {
		t.Fatalf("workspace must clear, got %q", cfg.ToolsWorkspace)
	}
}

func TestSettingsClearDefaultsAndChangeProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	svc := NewSettingsService(path)
	if err := svc.Save(SettingsInput{Provider: "kimi", Model: "k3", BaseURL: "https://example.invalid", APIKey: "fake-test-key"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Save(SettingsInput{Provider: "kimi"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := sidecar.LoadConfig(path)
	if cfg.Model != "" || cfg.BaseURL != "" || cfg.APIKey != "fake-test-key" {
		t.Fatal("empty overrides must restore defaults without clearing this provider's credential")
	}
	if err := svc.Save(SettingsInput{Provider: "deepseek"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = sidecar.LoadConfig(path)
	if cfg.APIKey != "" {
		t.Fatal("provider switch retained another provider's credential")
	}
}

func TestSettingsSwitchFromImplicitKimiClearsCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := sidecar.SaveConfig(path, sidecar.HostConfig{APIKey: "fake-implicit-kimi-key", Protocol: "anthropic", Auth: "bearer"}); err != nil {
		t.Fatal(err)
	}
	if err := NewSettingsService(path).Save(SettingsInput{Provider: "deepseek"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := sidecar.LoadConfig(path)
	if err != nil || cfg.APIKey != "" || cfg.Protocol != "" || cfg.Auth != "" {
		t.Fatal("implicit default provider credentials crossed into DeepSeek")
	}
}
