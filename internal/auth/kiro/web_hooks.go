package kiro

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/url"
	"strings"
)

// WebLoginHooks lets a non-interactive host drive SSO login: challenges are
// surfaced to the caller and manual inputs come from the caller's prompt.
type WebLoginHooks struct {
	Challenge func(url, userCode, verificationURI string)
	Prompt    func(prompt string) (string, error)
}

// SetWebLoginHooks installs hooks for web-driven login flows.
func (c *SSOOIDCClient) SetWebLoginHooks(hooks *WebLoginHooks) {
	if c == nil {
		return
	}
	c.webHooks = hooks
}

// notifyDeviceChallenge reports a device authorization challenge to the host.
func (c *SSOOIDCClient) notifyDeviceChallenge(resp *StartDeviceAuthResponse) bool {
	if c == nil || c.webHooks == nil || c.webHooks.Challenge == nil || resp == nil {
		return false
	}
	c.webHooks.Challenge(resp.VerificationURIComplete, resp.UserCode, resp.VerificationURI)
	return true
}

// notifyAuthCodeChallenge reports an authorization-code sign-in URL to the host.
func (c *SSOOIDCClient) notifyAuthCodeChallenge(authURL string) bool {
	if c == nil || c.webHooks == nil || c.webHooks.Challenge == nil {
		return false
	}
	c.webHooks.Challenge(authURL, "", "")
	return true
}

// startAuthCodePastePrompt accepts a callback URL pasted from a remote browser
// and forwards the parsed code into the callback channel.
func (c *SSOOIDCClient) startAuthCodePastePrompt(ctx context.Context, state string, resultChan chan<- AuthCodeCallbackResult) {
	if c == nil || c.webHooks == nil || c.webHooks.Prompt == nil {
		return
	}
	go func() {
		pasted, errPrompt := c.webHooks.Prompt("Open the Kiro sign-in URL, then paste the full callback URL from the browser address bar")
		if errPrompt != nil {
			return
		}
		result, errParse := ParseAuthCodeCallbackURL(pasted, state)
		if errParse != nil {
			select {
			case resultChan <- AuthCodeCallbackResult{Error: errParse.Error()}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case resultChan <- result:
		case <-ctx.Done():
		}
	}()
}

// ParseAuthCodeCallbackURL validates a pasted callback URL against the state
// generated for this login attempt.
func ParseAuthCodeCallbackURL(raw, expectedState string) (AuthCodeCallbackResult, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return AuthCodeCallbackResult{}, fmt.Errorf("empty callback URL")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://callback.invalid/?" + strings.TrimPrefix(trimmed, "?")
	}
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil {
		return AuthCodeCallbackResult{}, fmt.Errorf("invalid callback URL")
	}
	query := parsed.Query()
	code := query.Get("code")
	state := query.Get("state")
	if code == "" {
		return AuthCodeCallbackResult{}, fmt.Errorf("callback URL is missing code")
	}
	if expectedState == "" || subtle.ConstantTimeCompare([]byte(state), []byte(expectedState)) != 1 {
		return AuthCodeCallbackResult{}, fmt.Errorf("callback state does not match this login attempt")
	}
	return AuthCodeCallbackResult{Code: code, State: state}, nil
}
