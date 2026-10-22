package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPlusConfigV8RoundTrip(t *testing.T) {
	raw := []byte(`oauth:
  incognito-browser: true
  providers:
    kiro:
      fingerprint:
        kiro-version: custom
      preferred-endpoint: cli
credentials:
  kiro:
    - access-token: " token "
      region: " us-east-1 "
  bt:
    - phone: " 123 "
      password: " c2VjcmV0 "
`)
	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IncognitoBrowser || cfg.KiroPreferredEndpoint != "cli" || cfg.KiroFingerprint == nil || cfg.KiroFingerprint.KiroVersion != "custom" {
		t.Fatalf("lost Plus browser/fingerprint settings: %+v", cfg.KiroFingerprint)
	}
	if len(cfg.KiroKey) != 1 || cfg.KiroKey[0].AccessToken != "token" || cfg.KiroKey[0].Region != "us-east-1" {
		t.Fatal("Kiro credential did not load or normalize")
	}
	if len(cfg.BTKey) != 1 || cfg.BTKey[0].Phone != "123" || cfg.BTKey[0].Password != "c2VjcmV0" {
		t.Fatal("BT credential did not load or normalize")
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ParseConfigBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.KiroKey, reloaded.KiroKey) || !reflect.DeepEqual(cfg.BTKey, reloaded.BTKey) || !reflect.DeepEqual(cfg.KiroFingerprint, reloaded.KiroFingerprint) || cfg.KiroPreferredEndpoint != reloaded.KiroPreferredEndpoint || cfg.IncognitoBrowser != reloaded.IncognitoBrowser {
		t.Fatal("Plus settings changed during YAML round trip")
	}
	normalized, _, err := NormalizeConfigLayout(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(normalized); err != nil {
		t.Fatal(err)
	}
}
