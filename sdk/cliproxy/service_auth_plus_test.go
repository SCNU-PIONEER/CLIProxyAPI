package cliproxy

import "testing"

func TestDefaultAuthManagerRegistersPlusAuthenticators(t *testing.T) {
	manager := newDefaultAuthManager()
	if manager == nil {
		t.Fatal("newDefaultAuthManager returned nil")
	}
	configured := make(map[string]bool)
	for _, provider := range manager.ListProviders() {
		configured[provider.Key] = provider.Configured
	}
	for _, key := range []string{
		"codearts", "codebuddy", "codebuddy-ai", "cursor", "github-copilot",
		"gitlab", "iflow", "joycode", "kilo", "kiro", "qoder",
	} {
		if !configured[key] {
			t.Errorf("provider %q is not registered as configured", key)
		}
	}
}
