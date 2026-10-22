package codearts

import (
	"strings"
	"testing"
)

func TestRedactURLForLogDropsSessionSecrets(t *testing.T) {
	raw := "https://codearts.example.com/login?ticket_id=ticket-secret&state=abc"
	got := redactURLForLog(raw)
	if got != "https://codearts.example.com/login" {
		t.Fatalf("redactURLForLog() = %q, want scheme://host/path", got)
	}
	if strings.Contains(got, "ticket-secret") || strings.Contains(got, "ticket_id") {
		t.Fatalf("redacted URL still carries session material: %q", got)
	}
}

func TestRedactURLForLogHandlesUnparseableInput(t *testing.T) {
	if got := redactURLForLog("not a url"); got != "<redacted url>" {
		t.Fatalf("redactURLForLog() = %q, want <redacted url>", got)
	}
}
