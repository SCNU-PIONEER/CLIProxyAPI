package auth

import "testing"

func TestNotifyLoginChallengeDeliversStructuredChallenge(t *testing.T) {
	want := LoginChallenge{URL: "https://example.test/device", UserCode: "ABCD", VerificationURI: "https://example.test/device"}
	var got LoginChallenge
	opts := &LoginOptions{OnChallenge: func(challenge LoginChallenge) { got = challenge }}
	if !NotifyLoginChallenge(opts, want) || got != want {
		t.Fatalf("challenge not delivered: %+v", got)
	}
	if NotifyLoginChallenge(nil, want) || NotifyLoginChallenge(&LoginOptions{}, want) {
		t.Fatal("CLI callers without a callback must retain their interactive display")
	}
}
