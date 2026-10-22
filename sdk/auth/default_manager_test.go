package auth

import "testing"

func TestNewDefaultManagerRegistersAllBuiltInProviders(t *testing.T) {
	manager := NewDefaultManager(nil)
	if manager == nil {
		t.Fatal("NewDefaultManager returned nil")
	}
	configured := make(map[string]bool)
	for _, provider := range manager.ListProviders() {
		configured[provider.Key] = provider.Configured
	}
	expected := []string{
		"antigravity", "claude", "codex", "devin", "kimi", "kimi-ai", "kimi.ai",
		"meta", "xai",
		"codearts", "codebuddy", "codebuddy-ai", "cursor", "github-copilot",
		"gitlab", "iflow", "joycode", "kilo", "kiro", "qoder",
	}
	for _, key := range expected {
		if !configured[key] {
			t.Errorf("provider %q is not registered as configured", key)
		}
	}
	if len(manager.authenticators) != len(expected) {
		t.Errorf("registered authenticator count = %d, want %d", len(manager.authenticators), len(expected))
	}
}
