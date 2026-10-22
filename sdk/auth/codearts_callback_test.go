package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func codeArtsCallbackRequest(identifier, redirect string) *http.Request {
	target := "/callback?identifier=" + url.QueryEscape(identifier)
	if redirect != "" {
		target += "&redirect=" + url.QueryEscape(redirect)
	}
	return httptest.NewRequest(http.MethodGet, target, nil)
}

// TestHandleCodeArtsCallbackRejectsForgedRedirect ensures a local request that
// does not carry the ticket_id of this login attempt cannot complete the
// callback with an attacker-controlled identifier.
func TestHandleCodeArtsCallbackRejectsForgedRedirect(t *testing.T) {
	cbChan := make(chan codeartsCallbackResult, 1)
	forgedRedirect := "https://example.test/return?ticket_id=attacker-ticket"

	rec := httptest.NewRecorder()
	handleCodeArtsCallbackForTicket(rec, codeArtsCallbackRequest("attacker", forgedRedirect), "expected-ticket", cbChan)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("forged callback status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	select {
	case result := <-cbChan:
		if result.Identifier != "" {
			t.Fatalf("forged callback was accepted with identifier %q", result.Identifier)
		}
	default:
		t.Fatal("forged callback did not report an error")
	}
}

// TestHandleCodeArtsCallbackRejectsMissingRedirect ensures requests without the
// provider redirect are rejected rather than treated as a completed login.
func TestHandleCodeArtsCallbackRejectsMissingRedirect(t *testing.T) {
	cbChan := make(chan codeartsCallbackResult, 1)
	rec := httptest.NewRecorder()
	handleCodeArtsCallbackForTicket(rec, codeArtsCallbackRequest("attacker", ""), "expected-ticket", cbChan)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-redirect callback status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestHandleCodeArtsCallbackAcceptsMatchingTicket ensures the genuine provider
// redirect still completes the callback.
func TestHandleCodeArtsCallbackAcceptsMatchingTicket(t *testing.T) {
	cbChan := make(chan codeartsCallbackResult, 1)
	redirect := "https://example.test/return?ticket_id=expected-ticket"

	rec := httptest.NewRecorder()
	handleCodeArtsCallbackForTicket(rec, codeArtsCallbackRequest("user@example.test", redirect), "expected-ticket", cbChan)

	if rec.Code != http.StatusOK {
		t.Fatalf("valid callback status = %d, want %d", rec.Code, http.StatusOK)
	}
	select {
	case result := <-cbChan:
		if result.Identifier != "user@example.test" {
			t.Fatalf("valid callback identifier = %q, want %q", result.Identifier, "user@example.test")
		}
		if result.Error != "" {
			t.Fatalf("valid callback reported error %q", result.Error)
		}
	default:
		t.Fatal("valid callback did not record the identifier")
	}
}

// TestCodeArtsCallbackTicketMatches covers the redirect ticket comparison.
func TestCodeArtsCallbackTicketMatches(t *testing.T) {
	cases := []struct {
		name     string
		redirect string
		ticket   string
		want     bool
	}{
		{"matching", "https://example.test/return?ticket_id=abc", "abc", true},
		{"mismatch", "https://example.test/return?ticket_id=abc", "def", false},
		{"missing ticket in redirect", "https://example.test/return", "abc", false},
		{"no redirect", "", "abc", false},
		{"no ticket", "https://example.test/return?ticket_id=abc", "", false},
		{"malformed redirect", "://bad url", "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codeArtsCallbackTicketMatches(tc.redirect, tc.ticket); got != tc.want {
				t.Fatalf("codeArtsCallbackTicketMatches(%q, %q) = %v, want %v", tc.redirect, tc.ticket, got, tc.want)
			}
		})
	}
}
