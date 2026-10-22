package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "default", raw: "", want: "https://gitlab.com"},
		{name: "adds https", raw: "gitlab.example.com", want: "https://gitlab.example.com"},
		{name: "trims slash", raw: "https://gitlab.example.com/", want: "https://gitlab.example.com"},
		{name: "keeps private port", raw: "https://gitlab.internal:8443/", want: "https://gitlab.internal:8443"},
		{name: "rejects http", raw: "http://gitlab.example.com", wantErr: "must use https"},
		{name: "rejects other schemes", raw: "ftp://gitlab.example.com", wantErr: "must use https"},
		{name: "rejects userinfo", raw: "https://user:pass@gitlab.example.com", wantErr: "credentials"},
		{name: "rejects query", raw: "https://gitlab.example.com/?x=1", wantErr: "query"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateBaseURL(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ValidateBaseURL(%q) error = %v, want containing %q", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateBaseURL(%q) error = %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestGitLabCheckRedirect(t *testing.T) {
	first := func(raw string) []*http.Request {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		return []*http.Request{req}
	}
	next := func(raw string) *http.Request {
		return httptest.NewRequest(http.MethodGet, raw, nil)
	}

	if err := gitLabCheckRedirect(next("https://gitlab.com/next"), first("https://gitlab.com/start")); err != nil {
		t.Fatalf("same-origin https redirect rejected: %v", err)
	}
	if err := gitLabCheckRedirect(next("https://evil.example/next"), first("https://gitlab.com/start")); err == nil {
		t.Fatal("cross-origin redirect was accepted")
	}
	if err := gitLabCheckRedirect(next("http://gitlab.com/next"), first("https://gitlab.com/start")); err == nil {
		t.Fatal("http redirect was accepted")
	}
	if err := gitLabCheckRedirect(next("https://user:pass@gitlab.com/next"), first("https://gitlab.com/start")); err == nil {
		t.Fatal("redirect with userinfo was accepted")
	}
	via := make([]*http.Request, gitLabMaxRedirects)
	for i := range via {
		via[i] = httptest.NewRequest(http.MethodGet, "https://gitlab.com/start", nil)
	}
	if err := gitLabCheckRedirect(next("https://gitlab.com/next"), via); err == nil {
		t.Fatal("redirect chain longer than the limit was accepted")
	}
}

func TestGetCurrentUserRejectsInsecureBaseURL(t *testing.T) {
	client := NewAuthClient(nil)
	_, err := client.GetCurrentUser(context.Background(), "http://127.0.0.1:1", "token")
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("GetCurrentUser error = %v, want https rejection", err)
	}
}

func TestGetCurrentUserAllowsHTTPSPrivateOrigin(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":1,"username":"octocat","name":"Octo Cat"}`))
	}))
	defer server.Close()

	client := &AuthClient{httpClient: server.Client()}
	user, err := client.GetCurrentUser(context.Background(), server.URL, "token")
	if err != nil {
		t.Fatalf("GetCurrentUser error = %v", err)
	}
	if user.Username != "octocat" {
		t.Fatalf("username = %q, want octocat", user.Username)
	}
}

func TestGitLabClientRejectsCrossOriginRedirect(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"username":"attacker"}`))
	}))
	defer target.Close()

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v4/user", http.StatusFound)
	}))
	defer redirector.Close()

	client := redirector.Client()
	client.CheckRedirect = gitLabCheckRedirect
	authClient := &AuthClient{httpClient: client}
	_, err := authClient.GetCurrentUser(context.Background(), redirector.URL, "token")
	if err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("GetCurrentUser error = %v, want cross-origin rejection", err)
	}
}

func TestWaitForCallbackContextCancellation(t *testing.T) {
	server := NewOAuthServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := server.WaitForCallbackContext(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForCallbackContext error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("WaitForCallbackContext returned after %v, want immediate cancellation", elapsed)
	}
}
