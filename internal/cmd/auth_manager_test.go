package cmd

import (
	"testing"
)

// TestNewAuthManagerRegistersPlusAuthenticators guards the CLI login entrypoints
// for the providers added by the Plus merge. Every Do<Provider>Login helper in
// this package resolves its authenticator through newAuthManager; if a provider
// is missing there, the command fails at runtime with "authenticator not
// registered" even though the authenticator type exists.
func TestNewAuthManagerRegistersPlusAuthenticators(t *testing.T) {
	want := []string{
		"codearts",
		"codebuddy",
		"codebuddy-ai",
		"cursor",
		"github-copilot",
		"gitlab",
		"iflow",
		"joycode",
		"kilo",
		"kiro",
		"qoder",
	}

	configured := make(map[string]bool)
	for _, provider := range newAuthManager().ListProviders() {
		configured[provider.Key] = provider.Configured
	}

	for _, key := range want {
		if !configured[key] {
			t.Errorf("newAuthManager() does not register the %q authenticator; the matching login command would fail with \"authenticator not registered\"", key)
		}
	}
}
