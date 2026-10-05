package state

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAlwaysOnlineDefaultsToOff(t *testing.T) {
	var cfg Config
	cfg.SetDefaults()
	if cfg.WhatsApp.AlwaysOnline {
		t.Error("whatsapp.always_online must default to false")
	}

	// A config file that never mentions the key keeps it off.
	if err := yaml.Unmarshal([]byte("whatsapp:\n  session_name: x\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WhatsApp.AlwaysOnline {
		t.Error("an absent always_online key turned the option on")
	}
}

func TestAlwaysOnlineIsReadFromYaml(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("whatsapp:\n  always_online: true\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.WhatsApp.AlwaysOnline {
		t.Error("always_online: true was not read")
	}
}

func TestSampleConfigDocumentsAlwaysOnlineAsOff(t *testing.T) {
	body, err := os.ReadFile("../sample_config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		WhatsApp map[string]any `yaml:"whatsapp"`
	}
	if err := yaml.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	v, ok := raw.WhatsApp["always_online"]
	if !ok {
		t.Fatal("sample_config.yaml does not document whatsapp.always_online")
	}
	if v != false {
		t.Errorf("sample always_online = %v, want false", v)
	}
}
