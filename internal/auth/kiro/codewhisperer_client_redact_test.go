package kiro

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type staticRoundTripper func(*http.Request) (*http.Response, error)

func (f staticRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// TestGetUsageLimitsErrorRedactsResponseBody ensures API errors never embed
// raw provider bodies, which can carry access or refresh tokens.
func TestGetUsageLimitsErrorRedactsResponseBody(t *testing.T) {
	const marker = "secret-marker-body-token"
	client := &CodeWhispererClient{httpClient: &http.Client{Transport: staticRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"` + marker + `"}`)),
			Request:    request,
		}, nil
	})}}

	_, err := client.GetUsageLimits(context.Background(), "access-token", "client-id", "refresh-token", "")
	if err == nil {
		t.Fatalf("expected an error for a non-200 response")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("API error leaked response body: %v", err)
	}
	if !strings.Contains(err.Error(), "redacted") {
		t.Fatalf("API error did not describe the redacted body: %v", err)
	}
}
