package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func TestConfigFileAccessV8ManagementSecretPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const secret = "v8-load-test-secret"
	if err := os.WriteFile(path, []byte("server:\n  port: 8317\nmanagement:\n  secret-key: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	management, ok := document["management"].(map[string]any)
	if !ok {
		t.Fatal("v8 management mapping was not preserved")
	}
	if _, legacy := document["remote-management"]; legacy {
		t.Fatal("secret persistence introduced the legacy management mapping")
	}
	hash, ok := management["secret-key"].(string)
	if !ok || hash != cfg.RemoteManagement.SecretKey {
		t.Fatal("persisted and applied management secret hashes differ")
	}
	if err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)); err != nil {
		t.Fatalf("persisted management hash does not authenticate original secret: %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatal("plaintext management secret remained in the saved document")
	}
}
