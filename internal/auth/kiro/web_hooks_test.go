package kiro

import (
	"context"
	"testing"
	"time"
)

func TestParseAuthCodeCallbackURL(t *testing.T) {
	state := "expected-state"
	callbackURL := "http://127.0.0.1:9876/oauth/callback?code=code-1&state=" + state
	result, errParse := ParseAuthCodeCallbackURL(callbackURL, state)
	if errParse != nil {
		t.Fatalf("parse callback: %v", errParse)
	}
	if result.Code != "code-1" || result.State != state {
		t.Fatalf("unexpected result: %+v", result)
	}

	if _, errBare := ParseAuthCodeCallbackURL("code=code-1&state="+state, state); errBare != nil {
		t.Fatalf("parse bare query: %v", errBare)
	}
	if _, errState := ParseAuthCodeCallbackURL("http://127.0.0.1:9876/oauth/callback?code=code-1&state=other", state); errState == nil {
		t.Fatalf("state mismatch must be rejected")
	}
	if _, errCode := ParseAuthCodeCallbackURL("http://127.0.0.1:9876/oauth/callback?state="+state, state); errCode == nil {
		t.Fatalf("missing code must be rejected")
	}
	if _, errEmpty := ParseAuthCodeCallbackURL("", state); errEmpty == nil {
		t.Fatalf("empty URL must be rejected")
	}
}

func TestWebLoginHooksDeliverDeviceChallenge(t *testing.T) {
	client := &SSOOIDCClient{}
	var gotURL, gotUserCode, gotVerification string
	client.SetWebLoginHooks(&WebLoginHooks{
		Challenge: func(challengeURL, userCode, verificationURI string) {
			gotURL = challengeURL
			gotUserCode = userCode
			gotVerification = verificationURI
		},
	})
	handled := client.notifyDeviceChallenge(&StartDeviceAuthResponse{
		UserCode:                "ABCD-EFGH",
		VerificationURI:         "https://device.test/verify",
		VerificationURIComplete: "https://device.test/verify?user_code=ABCD-EFGH",
	})
	if !handled {
		t.Fatalf("device challenge was not handled by hooks")
	}
	if gotURL != "https://device.test/verify?user_code=ABCD-EFGH" || gotUserCode != "ABCD-EFGH" || gotVerification != "https://device.test/verify" {
		t.Fatalf("unexpected challenge: %q %q %q", gotURL, gotUserCode, gotVerification)
	}

	if client.notifyAuthCodeChallenge("https://auth.test/authorize") != true {
		t.Fatalf("auth code challenge was not handled")
	}
	if gotURL != "https://auth.test/authorize" || gotUserCode != "" {
		t.Fatalf("unexpected auth code challenge: %q %q", gotURL, gotUserCode)
	}
}

func TestAuthCodePastePromptFeedsCallbackChannel(t *testing.T) {
	state := "state-1"
	client := &SSOOIDCClient{}
	client.SetWebLoginHooks(&WebLoginHooks{
		Prompt: func(prompt string) (string, error) {
			return "http://127.0.0.1:9876/oauth/callback?code=code-2&state=" + state, nil
		},
	})
	resultChan := make(chan AuthCodeCallbackResult, 1)
	client.startAuthCodePastePrompt(context.Background(), state, resultChan)
	select {
	case result := <-resultChan:
		if result.Error != "" || result.Code != "code-2" {
			t.Fatalf("unexpected callback result: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("pasted callback was not delivered")
	}
}

func TestAuthCodePastePromptReportsMismatchedState(t *testing.T) {
	client := &SSOOIDCClient{}
	client.SetWebLoginHooks(&WebLoginHooks{
		Prompt: func(prompt string) (string, error) {
			return "http://127.0.0.1:9876/oauth/callback?code=code-2&state=other", nil
		},
	})
	resultChan := make(chan AuthCodeCallbackResult, 1)
	client.startAuthCodePastePrompt(context.Background(), "expected", resultChan)
	select {
	case result := <-resultChan:
		if result.Error == "" {
			t.Fatalf("mismatched state must surface an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("pasted callback error was not delivered")
	}
}
