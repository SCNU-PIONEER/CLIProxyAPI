package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codebuddy_ai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type fakeCodeBuddyLoginService struct {
	state   *codebuddy.AuthState
	storage *codebuddy.CodeBuddyTokenStorage
}

func (f *fakeCodeBuddyLoginService) FetchAuthState(context.Context) (*codebuddy.AuthState, error) {
	return f.state, nil
}

func (f *fakeCodeBuddyLoginService) PollForToken(context.Context, string) (*codebuddy.CodeBuddyTokenStorage, error) {
	return f.storage, nil
}

func TestCodeBuddyLoginDeliversChallengeToNonInteractiveHost(t *testing.T) {
	original := newCodeBuddyLoginService
	t.Cleanup(func() { newCodeBuddyLoginService = original })
	newCodeBuddyLoginService = func(*config.Config) codeBuddyLoginService {
		return &fakeCodeBuddyLoginService{
			state:   &codebuddy.AuthState{State: "cb-state", AuthURL: "https://codebuddy.example/login?state=cb-state"},
			storage: &codebuddy.CodeBuddyTokenStorage{AccessToken: "at", RefreshToken: "rt", UserID: "user-1", Domain: "codebuddy.example"},
		}
	}

	var challenges []LoginChallenge
	record, err := CodeBuddyAuthenticator{}.Login(context.Background(), &config.Config{}, &LoginOptions{
		NoBrowser:   true,
		OnChallenge: func(challenge LoginChallenge) { challenges = append(challenges, challenge) },
	})
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if len(challenges) != 1 || challenges[0].URL != "https://codebuddy.example/login?state=cb-state" {
		t.Fatalf("challenge not delivered, got %#v", challenges)
	}
	if record == nil || record.Provider != "codebuddy" {
		t.Fatalf("unexpected record: %#v", record)
	}
}

type fakeCodeBuddyAILoginService struct {
	state   *codebuddy_ai.AuthState
	storage *codebuddy_ai.CodeBuddyAITokenStorage
}

func (f *fakeCodeBuddyAILoginService) FetchAuthState(context.Context) (*codebuddy_ai.AuthState, error) {
	return f.state, nil
}

func (f *fakeCodeBuddyAILoginService) PollForToken(context.Context, string) (*codebuddy_ai.CodeBuddyAITokenStorage, error) {
	return f.storage, nil
}

func TestCodeBuddyAILoginDeliversChallengeToNonInteractiveHost(t *testing.T) {
	original := newCodeBuddyAILoginService
	t.Cleanup(func() { newCodeBuddyAILoginService = original })
	newCodeBuddyAILoginService = func(*config.Config) codeBuddyAILoginService {
		return &fakeCodeBuddyAILoginService{
			state:   &codebuddy_ai.AuthState{State: "cba-state", AuthURL: "https://codebuddy-ai.example/login?state=cba-state"},
			storage: &codebuddy_ai.CodeBuddyAITokenStorage{AccessToken: "at", RefreshToken: "rt", UserID: "user-2"},
		}
	}

	var challenges []LoginChallenge
	record, err := CodeBuddyAIAuthenticator{}.Login(context.Background(), &config.Config{}, &LoginOptions{
		NoBrowser:   true,
		OnChallenge: func(challenge LoginChallenge) { challenges = append(challenges, challenge) },
	})
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if len(challenges) != 1 || challenges[0].URL != "https://codebuddy-ai.example/login?state=cba-state" {
		t.Fatalf("challenge not delivered, got %#v", challenges)
	}
	if record == nil || record.Provider != "codebuddy-ai" {
		t.Fatalf("unexpected record: %#v", record)
	}
}
