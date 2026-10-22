package auth

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseCodeArtsCallbackURL(t *testing.T) {
	ticketID := "ticket-abc123"
	redirect := "https://provider.test/authorize?ticket_id=" + ticketID
	callbackURL := "http://127.0.0.1:41234/callback?identifier=ident-1&redirect=" + url.QueryEscape(redirect)

	result, errParse := parseCodeArtsCallbackURL(callbackURL, ticketID)
	if errParse != nil {
		t.Fatalf("parse callback: %v", errParse)
	}
	if result.Identifier != "ident-1" {
		t.Fatalf("identifier=%q", result.Identifier)
	}
	if result.Redirect != redirect {
		t.Fatalf("redirect=%q", result.Redirect)
	}

	bareQuery := strings.TrimPrefix(callbackURL, "http://127.0.0.1:41234/callback?")
	if _, errBare := parseCodeArtsCallbackURL(bareQuery, ticketID); errBare != nil {
		t.Fatalf("parse bare query: %v", errBare)
	}

	wrongTicket := "http://127.0.0.1:41234/callback?identifier=ident-1&redirect=" + url.QueryEscape("https://provider.test/authorize?ticket_id=other")
	if _, errWrong := parseCodeArtsCallbackURL(wrongTicket, ticketID); errWrong == nil {
		t.Fatalf("mismatched ticket must be rejected")
	}

	if _, errEmpty := parseCodeArtsCallbackURL("", ticketID); errEmpty == nil {
		t.Fatalf("empty callback URL must be rejected")
	}
	if _, errMissing := parseCodeArtsCallbackURL("http://127.0.0.1:41234/callback?foo=bar", ticketID); errMissing == nil {
		t.Fatalf("callback without identifier/redirect must be rejected")
	}
}

func TestParseJoyCodeCallbackURL(t *testing.T) {
	authKey := "joy-auth-key"
	callbackURL := "http://127.0.0.1:41235/?authKey=" + authKey + "&pt_key=pt-1"
	result, errParse := parseJoyCodeCallbackURL(callbackURL, authKey)
	if errParse != nil {
		t.Fatalf("parse callback: %v", errParse)
	}
	if result.PTKey != "pt-1" || result.Error != "" {
		t.Fatalf("unexpected result: %+v", result)
	}

	camelCase := "http://127.0.0.1:41235/joycode/callback?authKey=" + authKey + "&ptKey=pt-2"
	resultCamel, errCamel := parseJoyCodeCallbackURL(camelCase, authKey)
	if errCamel != nil || resultCamel.PTKey != "pt-2" {
		t.Fatalf("camelCase result=%+v err=%v", resultCamel, errCamel)
	}

	wrongKey := "http://127.0.0.1:41235/?authKey=other&pt_key=pt-1"
	if _, errWrong := parseJoyCodeCallbackURL(wrongKey, authKey); errWrong == nil {
		t.Fatalf("mismatched authKey must be rejected")
	}

	missingKey := "http://127.0.0.1:41235/?pt_key=pt-1"
	if _, errMissingKey := parseJoyCodeCallbackURL(missingKey, authKey); errMissingKey == nil {
		t.Fatalf("missing authKey must be rejected")
	}

	missingPTKey := "http://127.0.0.1:41235/?authKey=" + authKey
	if _, errMissingPTKey := parseJoyCodeCallbackURL(missingPTKey, authKey); errMissingPTKey == nil {
		t.Fatalf("missing pt_key must be rejected")
	}
}
