package cursor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRefreshTokenWithClientUsesSuppliedClient(t *testing.T) {
	used := false
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		used = true
		if got := req.Header.Get("Authorization"); got != "Bearer refresh-token" {
			t.Fatalf("Authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"accessToken":"new-access","refreshToken":"new-refresh"}`)),
			Request:    req,
		}, nil
	})}

	tokens, err := RefreshTokenWithClient(context.Background(), "refresh-token", client)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if !used {
		t.Fatal("supplied client was not used")
	}
	if tokens.AccessToken != "new-access" || tokens.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %+v", tokens)
	}
}

func TestRefreshTokenWithClientKeepsRefreshTokenWhenMissing(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"accessToken":"new-access"}`)),
			Request:    req,
		}, nil
	})}

	tokens, err := RefreshTokenWithClient(context.Background(), "original-refresh", client)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if tokens.RefreshToken != "original-refresh" {
		t.Fatalf("refresh token = %q, want original", tokens.RefreshToken)
	}
}
