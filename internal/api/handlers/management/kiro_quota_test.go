package management

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type quotaRefreshExecutor struct {
	coreauth.ProviderExecutor
	called bool
}

func (e *quotaRefreshExecutor) Identifier() string { return "kiro" }
func (e *quotaRefreshExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.called = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "rotated-access"
	updated.Metadata["refresh_token"] = "rotated-refresh"
	updated.Metadata["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return updated, nil
}

func TestKiroQuotaRefreshUsesManagerAndCallerContext(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &quotaRefreshExecutor{}
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "quota-test", Provider: "kiro", Metadata: map[string]any{
		"access_token": "old", "refresh_token": "old-refresh",
		"expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
	}}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager}
	got := h.ensureKiroTokenFresh(t.Context(), auth, extractKiroTokenData(auth))
	if got == nil || got.AccessToken != "rotated-access" || !executor.called {
		t.Fatalf("quota did not use canonical refresh: %+v", got)
	}
	stored, ok := manager.GetByID(auth.ID)
	if !ok || stored.Metadata["refresh_token"] != "rotated-refresh" {
		t.Fatal("quota refresh lost the rotated token instead of updating the manager")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := h.forceRefreshKiroToken(ctx, auth); got != nil {
		t.Fatal("canceled quota request refreshed credentials")
	}
}

type quotaRoundTripper func(*http.Request) (*http.Response, error)

func (f quotaRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestKiroQuotaResponseDoesNotExposeProviderErrors(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(t.Context(), &coreauth.Auth{ID: "quota-http", Provider: "kiro", Metadata: map[string]any{"access_token": "secret-access"}}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = quotaRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer secret-access" {
			t.Fatal("quota request lost the credential")
		}
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("SECRET-UPSTREAM-BODY")), Request: req}, nil
	})
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetKiroQuota(ctx)
	if rr.Code != http.StatusBadGateway || strings.Contains(rr.Body.String(), "SECRET-UPSTREAM-BODY") {
		t.Fatalf("unsafe quota error: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestKiroQuotaExtractAllowsRefreshOnlyCredentials(t *testing.T) {
	got := extractKiroTokenData(&coreauth.Auth{Metadata: map[string]any{"refresh_token": "refresh-only"}})
	if got == nil || got.RefreshToken != "refresh-only" {
		t.Fatal("refresh-only configuration cannot be activated by quota check")
	}
}
