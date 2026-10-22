package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestGitLabLoginPATRequiresHTTPS ensures the shared login entry point rejects
// a plaintext base URL before any credential is sent.
func TestGitLabLoginPATRequiresHTTPS(t *testing.T) {
	authenticator := NewGitLabAuthenticator()
	opts := &LoginOptions{
		NoBrowser: true,
		Metadata: map[string]string{
			gitLabLoginModeMetadataKey:           gitLabLoginModePAT,
			gitLabBaseURLMetadataKey:             "http://gitlab.example.com",
			gitLabPersonalAccessTokenMetadataKey: "test-token",
		},
	}

	_, err := authenticator.Login(context.Background(), &config.Config{}, opts)
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("Login error = %v, want https rejection", err)
	}
}

// TestGitLabLoginPATRejectsCredentialsInBaseURL ensures base URLs with
// embedded credentials are rejected before use.
func TestGitLabLoginPATRejectsCredentialsInBaseURL(t *testing.T) {
	authenticator := NewGitLabAuthenticator()
	opts := &LoginOptions{
		NoBrowser: true,
		Metadata: map[string]string{
			gitLabLoginModeMetadataKey:           gitLabLoginModePAT,
			gitLabBaseURLMetadataKey:             "https://user:pass@gitlab.example.com",
			gitLabPersonalAccessTokenMetadataKey: "test-token",
		},
	}

	_, err := authenticator.Login(context.Background(), &config.Config{}, opts)
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("Login error = %v, want userinfo rejection", err)
	}
}
