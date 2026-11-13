package sidecar

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigMissingFileIsZero(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if c != (HostConfig{}) {
		t.Fatalf("config = %+v, want zero", c)
	}
}

func TestLoadConfigRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-sidecar.toml")
	body := "# comment\n" +
		"provider = \"kimi\"\n" +
		"api_key = 'sk-test'\n" +
		"base_url = \"https://api.example.com\"\n" +
		"model = \"k3\"\n" +
		"session_id = \"s-1\"\n" +
		"future_key = \"ignored\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "kimi" || c.APIKey != "sk-test" || c.BaseURL != "https://api.example.com" || c.Model != "k3" || c.SessionID != "s-1" {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadConfigMalformedIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-sidecar.toml")
	if err := os.WriteFile(path, []byte("api_key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected an error for a line without =")
	}
}

func TestWriteConfigTemplateNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-sidecar.toml")
	if err := os.WriteFile(path, []byte("provider = \"deepseek\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigTemplate(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "provider = \"deepseek\"\n" {
		t.Fatalf("template overwrote an existing config: %q", b)
	}

	// A fresh path gets the template with no credentials baked in.
	fresh := filepath.Join(t.TempDir(), "app-sidecar.toml")
	if err := WriteConfigTemplate(fresh); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "" {
		t.Fatalf("template must not carry credentials: %+v", c)
	}
}
