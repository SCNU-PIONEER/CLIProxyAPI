package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codebuddy_ai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type CodeBuddyAIAuthenticator struct{}

func NewCodeBuddyAIAuthenticator() Authenticator {
	return &CodeBuddyAIAuthenticator{}
}

func (CodeBuddyAIAuthenticator) Provider() string {
	return "codebuddy-ai"
}

var codeBuddyAIRefreshLead = 24 * time.Hour

func (CodeBuddyAIAuthenticator) RefreshLead() *time.Duration {
	return &codeBuddyAIRefreshLead
}

// codeBuddyAILoginService is the subset of the CodeBuddy AI auth client used by Login.
type codeBuddyAILoginService interface {
	FetchAuthState(ctx context.Context) (*codebuddy_ai.AuthState, error)
	PollForToken(ctx context.Context, state string) (*codebuddy_ai.CodeBuddyAITokenStorage, error)
}

// newCodeBuddyAILoginService builds the CodeBuddy AI auth client; overridden in tests.
var newCodeBuddyAILoginService = func(cfg *config.Config) codeBuddyAILoginService {
	return codebuddy_ai.NewCodeBuddyAIAuth(cfg)
}

func (a CodeBuddyAIAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("codebuddy-ai: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	authSvc := newCodeBuddyAILoginService(cfg)

	authState, err := authSvc.FetchAuthState(ctx)
	if err != nil {
		return nil, fmt.Errorf("codebuddy-ai: failed to fetch auth state: %w", err)
	}

	if !NotifyLoginChallenge(opts, LoginChallenge{URL: authState.AuthURL}) {
		fmt.Printf("\nPlease open the following URL in your browser to login:\n\n  %s\n\n", authState.AuthURL)
	}
	fmt.Println("Waiting for authorization...")

	if !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(authState.AuthURL); errOpen != nil {
				log.Debugf("codebuddy-ai: failed to open browser: %v", errOpen)
			}
		}
	}

	storage, err := authSvc.PollForToken(ctx, authState.State)
	if err != nil {
		return nil, fmt.Errorf("codebuddy-ai: %s: %w", codebuddy_ai.GetUserFriendlyMessage(err), err)
	}

	fmt.Printf("\nSuccessfully logged in! (User ID: %s)\n", storage.UserID)

	authID := fmt.Sprintf("codebuddy-ai-%s.json", sanitizeProviderFileNamePart(storage.UserID))

	label := storage.UserID
	if label == "" {
		label = "codebuddy-ai-user"
	}

	return &coreauth.Auth{
		ID:       authID,
		Provider: a.Provider(),
		FileName: authID,
		Label:    label,
		Storage:  storage,
		Metadata: map[string]any{
			"access_token":  storage.AccessToken,
			"refresh_token": storage.RefreshToken,
			"user_id":       storage.UserID,
			"domain":        storage.Domain,
			"expires_in":    storage.ExpiresIn,
		},
	}, nil
}
