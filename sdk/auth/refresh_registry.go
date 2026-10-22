package auth

import (
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func init() {
	registerRefreshLead("codex", func() Authenticator { return NewCodexAuthenticator() })
	registerRefreshLead("claude", func() Authenticator { return NewClaudeAuthenticator() })
	registerRefreshLead("antigravity", func() Authenticator { return NewAntigravityAuthenticator() })
	registerRefreshLead("kimi", func() Authenticator { return NewKimiAuthenticator() })
	registerRefreshLead("kimi-ai", func() Authenticator { return NewKimiAIAuthenticator() })
	registerRefreshLead("kimi.ai", func() Authenticator { return NewKimiAIDotAuthenticator() })
	registerRefreshLead("xai", func() Authenticator { return NewXAIAuthenticator() })
	registerRefreshLead("devin", func() Authenticator { return NewDevinAuthenticator() })
	registerRefreshLead("meta", func() Authenticator { return NewMetaAuthenticator() })

	// Plus providers must be registered too: the automatic refresh loop skips
	// any auth whose ProviderRefreshLead is nil, so a missing registration means
	// the provider is never refreshed even when its executor can refresh.
	registerRefreshLead("codearts", func() Authenticator { return NewCodeArtsAuthenticator() })
	registerRefreshLead("codebuddy", func() Authenticator { return NewCodeBuddyAuthenticator() })
	registerRefreshLead("codebuddy-ai", func() Authenticator { return NewCodeBuddyAIAuthenticator() })
	registerRefreshLead("cursor", func() Authenticator { return NewCursorAuthenticator() })
	registerRefreshLead("github-copilot", func() Authenticator { return NewGitHubCopilotAuthenticator() })
	registerRefreshLead("gitlab", func() Authenticator { return NewGitLabAuthenticator() })
	registerRefreshLead("iflow", func() Authenticator { return NewIFlowAuthenticator() })
	registerRefreshLead("joycode", func() Authenticator { return NewJoyCodeAuthenticator() })
	registerRefreshLead("kilo", func() Authenticator { return NewKiloAuthenticator() })
	registerRefreshLead("kiro", func() Authenticator { return NewKiroAuthenticator() })
	registerRefreshLead("qoder", func() Authenticator { return NewQoderAuthenticator() })

}

func registerRefreshLead(provider string, factory func() Authenticator) {
	cliproxyauth.RegisterRefreshLeadProvider(provider, func() *time.Duration {
		if factory == nil {
			return nil
		}
		auth := factory()
		if auth == nil {
			return nil
		}
		return auth.RefreshLead()
	})
}
