package kiro

import (
	"fmt"
	"strings"
	"testing"
)

func TestRedactResponseBodyForLogHidesTokenMaterial(t *testing.T) {
	body := []byte(`{"access_token":"super-secret","refresh_token":"also-secret"}`)
	got := redactResponseBodyForLog(body)
	if strings.Contains(got, "super-secret") || strings.Contains(got, "also-secret") {
		t.Fatalf("redacted body leaked token material: %q", got)
	}
	if want := fmt.Sprintf("<redacted %d bytes>", len(body)); got != want {
		t.Fatalf("redacted body = %q, want %q", got, want)
	}
}
