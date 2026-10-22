package auth

import (
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestProviderRefreshLeads(t *testing.T) {
	tests := []struct {
		name          string
		authenticator Authenticator
		want          time.Duration
		wantNil       bool
	}{
		{name: "codex", authenticator: NewCodexAuthenticator(), want: 96 * time.Hour},
		{name: "claude", authenticator: NewClaudeAuthenticator(), want: 4 * time.Hour},
		{name: "antigravity", authenticator: NewAntigravityAuthenticator(), want: 30 * time.Minute},
		{name: "kimi", authenticator: NewKimiAuthenticator(), want: 5 * time.Minute},
		{name: "kimi-ai", authenticator: NewKimiAIAuthenticator(), want: 5 * time.Minute},
		{name: "kimi.ai", authenticator: NewKimiAIDotAuthenticator(), want: 5 * time.Minute},
		{name: "xai", authenticator: NewXAIAuthenticator(), want: 5 * time.Minute},
		{name: "devin", authenticator: NewDevinAuthenticator(), wantNil: true},
		{name: "meta", authenticator: NewMetaAuthenticator(), wantNil: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.authenticator.Provider(); got != test.name {
				t.Fatalf("Provider() = %q, want %q", got, test.name)
			}
			lead := test.authenticator.RefreshLead()
			if test.wantNil {
				if lead != nil {
					t.Fatalf("RefreshLead() = %v, want nil", lead)
				}
				return
			}
			if lead == nil || *lead != test.want {
				t.Fatalf("RefreshLead() = %v, want %v", lead, test.want)
			}
		})
	}
}

// TestRefreshRegistryRegistersAllBuiltinLeads guards the scheduler integration:
// the automatic refresh loop skips any auth whose ProviderRefreshLead is nil, so
// every built-in authenticator factory (including the Kimi aliases) must be
// registered with its own RefreshLead value.
func TestRefreshRegistryRegistersAllBuiltinLeads(t *testing.T) {
	tests := []struct {
		provider      string
		authenticator Authenticator
	}{
		{provider: "codex", authenticator: NewCodexAuthenticator()},
		{provider: "claude", authenticator: NewClaudeAuthenticator()},
		{provider: "antigravity", authenticator: NewAntigravityAuthenticator()},
		{provider: "kimi", authenticator: NewKimiAuthenticator()},
		{provider: "kimi-ai", authenticator: NewKimiAIAuthenticator()},
		{provider: "kimi.ai", authenticator: NewKimiAIDotAuthenticator()},
		{provider: "xai", authenticator: NewXAIAuthenticator()},
		{provider: "devin", authenticator: NewDevinAuthenticator()},
		{provider: "meta", authenticator: NewMetaAuthenticator()},
		{provider: "codearts", authenticator: NewCodeArtsAuthenticator()},
		{provider: "codebuddy", authenticator: NewCodeBuddyAuthenticator()},
		{provider: "codebuddy-ai", authenticator: NewCodeBuddyAIAuthenticator()},
		{provider: "cursor", authenticator: NewCursorAuthenticator()},
		{provider: "github-copilot", authenticator: NewGitHubCopilotAuthenticator()},
		{provider: "gitlab", authenticator: NewGitLabAuthenticator()},
		{provider: "iflow", authenticator: NewIFlowAuthenticator()},
		{provider: "joycode", authenticator: NewJoyCodeAuthenticator()},
		{provider: "kilo", authenticator: NewKiloAuthenticator()},
		{provider: "kiro", authenticator: NewKiroAuthenticator()},
		{provider: "qoder", authenticator: NewQoderAuthenticator()},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			if got := test.authenticator.Provider(); got != test.provider {
				t.Fatalf("Provider() = %q, want %q", got, test.provider)
			}
			want := test.authenticator.RefreshLead()
			got := cliproxyauth.ProviderRefreshLead(test.provider, nil)
			if (want == nil) != (got == nil) {
				t.Fatalf("ProviderRefreshLead() = %v, authenticator lead = %v", got, want)
			}
			if want != nil && *got != *want {
				t.Fatalf("ProviderRefreshLead() = %s, authenticator lead = %s", *got, *want)
			}
		})
	}
}

// TestRefreshRegistryDoesNotScheduleBTGlobally pins the policy: BT scheduling is
// per-auth (refresh_interval_seconds on config activation), so a global BT lead
// must not exist and file-backed BT auths stay unscheduled.
func TestRefreshRegistryDoesNotScheduleBTGlobally(t *testing.T) {
	if lead := cliproxyauth.ProviderRefreshLead("bt", nil); lead != nil {
		t.Fatalf("global bt refresh lead = %s, want none", *lead)
	}
}
