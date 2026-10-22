package auth

import coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"

// NewDefaultManager builds the default auth manager with every built-in
// authenticator: the target provider set plus the Plus providers ported from
// the source fork. Callers that need a custom authenticator set can still use
// NewManager directly.
func NewDefaultManager(store coreauth.Store) *Manager {
	return NewManager(store,
		NewCodexAuthenticator(),
		NewClaudeAuthenticator(),
		NewAntigravityAuthenticator(),
		NewKimiAuthenticator(),
		NewKimiAIAuthenticator(),
		NewKimiAIDotAuthenticator(),
		NewXAIAuthenticator(),
		NewDevinAuthenticator(),
		NewMetaAuthenticator(),
		NewCodeArtsAuthenticator(),
		NewCodeBuddyAuthenticator(),
		NewCodeBuddyAIAuthenticator(),
		NewCursorAuthenticator(),
		NewGitHubCopilotAuthenticator(),
		NewGitLabAuthenticator(),
		NewIFlowAuthenticator(),
		NewJoyCodeAuthenticator(),
		NewKiloAuthenticator(),
		NewKiroAuthenticator(),
		NewQoderAuthenticator(),
	)
}
