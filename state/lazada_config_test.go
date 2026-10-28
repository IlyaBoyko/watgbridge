package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLazadaDefaults(t *testing.T) {
	var cfg Config
	cfg.SetDefaults()
	lz := cfg.Lazada
	if lz.Enabled {
		t.Error("lazada must default to off")
	}
	if lz.APIURL != "https://api.lazada.com.my/rest" || lz.AuthURL != "https://auth.lazada.com/rest" {
		t.Errorf("urls = %q, %q", lz.APIURL, lz.AuthURL)
	}
	if lz.SafetyPollSeconds != 180 || lz.ActivePollSeconds != 15 || lz.TextLimit != 1000 {
		t.Errorf("intervals = %d, %d, %d", lz.SafetyPollSeconds, lz.ActivePollSeconds, lz.TextLimit)
	}
}

func writeConfig(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Path: path}
	cfg.SetDefaults()
	return cfg
}

func TestLazadaDefaultsSurviveAConfigWithoutTheBlock(t *testing.T) {
	cfg := writeConfig(t, "time_zone: UTC\n")
	if err := cfg.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.Lazada.SafetyPollSeconds != 180 || cfg.Lazada.TextLimit != 1000 || cfg.Lazada.APIURL == "" {
		t.Errorf("defaults lost: %+v", cfg.Lazada)
	}
}

func TestLazadaEnabledWithoutCredentialsFailsStartup(t *testing.T) {
	for name, body := range map[string]string{
		"no key":    "lazada:\n  enabled: true\n  app_secret: s\n",
		"no secret": "lazada:\n  enabled: true\n  app_key: k\n",
		"neither":   "lazada:\n  enabled: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			err := writeConfig(t, body).LoadConfig()
			if err == nil || !strings.Contains(err.Error(), "lazada") {
				t.Fatalf("err = %v, want a lazada startup error", err)
			}
		})
	}
}

func TestLazadaErrorNeverContainsTheSecret(t *testing.T) {
	err := writeConfig(t, "lazada:\n  enabled: true\n  app_secret: TOPSECRET\n").LoadConfig()
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("err = %v", err)
	}
}

func TestLazadaDisabledChecksNothing(t *testing.T) {
	// A partial block is fine while the channel is off.
	if err := writeConfig(t, "lazada:\n  enabled: false\n  app_key: k\n").LoadConfig(); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestLazadaEnabledWithCredentialsLoads(t *testing.T) {
	cfg := writeConfig(t, "lazada:\n  enabled: true\n  app_key: k\n  app_secret: s\n  active_poll_seconds: 0\n  text_limit: 500\n")
	if err := cfg.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	// 0 means "off" for the active poll and must not be turned back into 15.
	if cfg.Lazada.ActivePollSeconds != 0 || cfg.Lazada.TextLimit != 500 || cfg.Lazada.SafetyPollSeconds != 180 {
		t.Errorf("%+v", cfg.Lazada)
	}
}

func TestLazadaBlankValuesFallBackToDefaults(t *testing.T) {
	cfg := writeConfig(t, "lazada:\n  api_url: \"\"\n  safety_poll_seconds: 0\n  text_limit: -1\n")
	if err := cfg.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.Lazada.APIURL == "" || cfg.Lazada.SafetyPollSeconds != 180 || cfg.Lazada.TextLimit != 1000 {
		t.Errorf("%+v", cfg.Lazada)
	}
}

func TestSampleConfigDocumentsLazadaAsOff(t *testing.T) {
	body, err := os.ReadFile("../sample_config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Lazada map[string]any `yaml:"lazada"`
	}
	if err := yaml.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw.Lazada["enabled"]; !ok || v != false {
		t.Errorf("sample lazada.enabled = %v (documented: %v), want false", v, ok)
	}
	for _, key := range []string{"app_key", "app_secret", "api_url", "auth_url", "safety_poll_seconds", "active_poll_seconds", "text_limit"} {
		if _, ok := raw.Lazada[key]; !ok {
			t.Errorf("sample_config.yaml does not document lazada.%s", key)
		}
	}
	// The sample must load cleanly into the real struct.
	var cfg Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateLazada(); err != nil {
		t.Errorf("sample config fails validation: %v", err)
	}
}

func TestLazadaCallbackURL(t *testing.T) {
	var cfg Config
	cfg.SetDefaults()
	if cfg.Lazada.CallbackURL != "https://clarusmolecular.com/" {
		t.Errorf("default = %q", cfg.Lazada.CallbackURL)
	}

	// A config without the key keeps the default; a blank one is refilled.
	for name, body := range map[string]string{"absent": "time_zone: UTC\n", "blank": "lazada:\n  callback_url: \"\"\n"} {
		c := writeConfig(t, body)
		if err := c.LoadConfig(); err != nil {
			t.Fatal(err)
		}
		if c.Lazada.CallbackURL != "https://clarusmolecular.com/" {
			t.Errorf("%s: callback_url = %q", name, c.Lazada.CallbackURL)
		}
	}

	c := writeConfig(t, "lazada:\n  callback_url: https://example.com/auth\n")
	if err := c.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if c.Lazada.CallbackURL != "https://example.com/auth" {
		t.Errorf("callback_url = %q", c.Lazada.CallbackURL)
	}
}
