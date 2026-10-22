package auth

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// qoderCallbackURL builds the encoded forward URL the VBS handler would send.
func qoderCallbackURL(token string) string {
	return "qoder://aicoding.aicoding-agent/login-success?tokenString=" + url.QueryEscape(token) + "&auth=" + url.QueryEscape("auth-field")
}

// TestQoderCallbackServerRejectsUnknownState ensures the loopback listener only
// serves the per-login path: a local page that does not know the random state
// cannot push a token into the pending login.
func TestQoderCallbackServerRejectsUnknownState(t *testing.T) {
	srv, port, resultCh, err := startQoderCallbackServer(0, "known-state", "expected-nonce")
	if err != nil {
		t.Fatalf("startQoderCallbackServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	raw := qoderCallbackURL("injected-token")
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/forward?url=%s", port, url.QueryEscape(raw)))
	if err != nil {
		t.Fatalf("GET unknown callback path: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown callback path returned %d, want 404", resp.StatusCode)
	}

	select {
	case res := <-resultCh:
		t.Fatalf("unknown callback path injected a result: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestQoderCallbackServerAcceptsBoundState ensures the expected per-login path
// still delivers the token.
func TestQoderCallbackServerAcceptsBoundState(t *testing.T) {
	srv, port, resultCh, err := startQoderCallbackServer(0, "known-state", "expected-nonce")
	if err != nil {
		t.Fatalf("startQoderCallbackServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	raw := qoderCallbackURL("callback-token")
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/forward/known-state?url=%s", port, url.QueryEscape(raw)))
	if err != nil {
		t.Fatalf("GET bound callback path: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bound callback path returned %d, want 200", resp.StatusCode)
	}

	select {
	case res := <-resultCh:
		if res.TokenString != "callback-token" {
			t.Fatalf("token = %q, want callback-token", res.TokenString)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bound callback did not deliver a result")
	}
}

// TestQoderCallbackServerDoesNotBlockOnDuplicateCallbacks ensures a second
// callback cannot wedge the handler goroutine.
func TestQoderCallbackServerDoesNotBlockOnDuplicateCallbacks(t *testing.T) {
	srv, port, _, err := startQoderCallbackServer(0, "known-state", "expected-nonce")
	if err != nil {
		t.Fatalf("startQoderCallbackServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	raw := qoderCallbackURL("callback-token")
	for i := 0; i < 2; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/forward/known-state?url=%s", port, url.QueryEscape(raw)))
		if err != nil {
			t.Fatalf("GET duplicate callback: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// TestParseQoderCallbackURLStateAndNonce verifies that callbacks echoing the
// login nonce/state must match it, while provider callbacks that omit both are
// still accepted.
func TestParseQoderCallbackURLStateAndNonce(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		nonce   string
		state   string
		wantOK  bool
		wantTok string
	}{
		{
			name:    "matching nonce and state",
			raw:     "qoder://aicoding.aicoding-agent/login-success?tokenString=tok&auth=a&nonce=nonce-1&state=state-1",
			nonce:   "nonce-1",
			state:   "state-1",
			wantOK:  true,
			wantTok: "tok",
		},
		{
			name:  "nonce mismatch",
			raw:   "qoder://aicoding.aicoding-agent/login-success?tokenString=tok&auth=a&nonce=other",
			nonce: "nonce-1",
			state: "state-1",
		},
		{
			name:  "state mismatch",
			raw:   "qoder://aicoding.aicoding-agent/login-success?tokenString=tok&auth=a&state=other",
			nonce: "nonce-1",
			state: "state-1",
		},
		{
			name:    "provider does not echo nonce or state",
			raw:     "qoder://aicoding.aicoding-agent/login-success?tokenString=tok&auth=a",
			nonce:   "nonce-1",
			state:   "state-1",
			wantOK:  true,
			wantTok: "tok",
		},
		{
			name:    "bare query string",
			raw:     "tokenString=tok&auth=a",
			nonce:   "nonce-1",
			state:   "state-1",
			wantOK:  true,
			wantTok: "tok",
		},
		{
			name:  "missing token",
			raw:   "qoder://aicoding.aicoding-agent/login-success?auth=a",
			nonce: "nonce-1",
			state: "state-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := parseQoderCallbackURL(tc.raw, tc.nonce, tc.state)
			if ok != tc.wantOK {
				t.Fatalf("parseQoderCallbackURL(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			}
			if ok && res.TokenString != tc.wantTok {
				t.Fatalf("token = %q, want %q", res.TokenString, tc.wantTok)
			}
			if ok && strings.TrimSpace(res.TokenString) == "" {
				t.Fatal("accepted callback returned an empty token")
			}
		})
	}
}
