package joycode

import (
	"strings"
	"testing"
)

func TestRedactURLForLogDropsSessionSecrets(t *testing.T) {
	raw := "https://joycode.jd.com/login/?ideAppName=JoyCode&authKey=secret-auth-key&state=abc"
	got := redactURLForLog(raw)
	if got != "https://joycode.jd.com/login/" {
		t.Fatalf("redactURLForLog() = %q, want scheme://host/path", got)
	}
	if strings.Contains(got, "secret-auth-key") || strings.Contains(got, "authKey") {
		t.Fatalf("redacted URL still carries session material: %q", got)
	}
}

func TestRedactURLForLogHandlesUnparseableInput(t *testing.T) {
	if got := redactURLForLog("not a url"); got != "<redacted url>" {
		t.Fatalf("redactURLForLog() = %q, want <redacted url>", got)
	}
}
