package auth

// LoginChallenge contains only the browser-facing portion of a login exchange.
// Device secrets, PKCE verifiers and access tokens must never be included.
type LoginChallenge struct {
	URL             string `json:"url,omitempty"`
	UserCode        string `json:"user_code,omitempty"`
	VerificationURI string `json:"verification_uri,omitempty"`
}

// NotifyLoginChallenge delivers a challenge to a non-interactive host. A true
// result tells the caller not to print the challenge to process logs/stdout.
// Callbacks execute synchronously and should not block the authentication flow.
func NotifyLoginChallenge(opts *LoginOptions, challenge LoginChallenge) bool {
	if opts == nil || opts.OnChallenge == nil {
		return false
	}
	opts.OnChallenge(challenge)
	return true
}
