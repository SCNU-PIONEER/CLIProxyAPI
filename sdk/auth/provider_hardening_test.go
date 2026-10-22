package auth

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeProviderFileNamePartRejectsPathSeparators ensures provider
// identifiers cannot escape the auth directory through credential file names.
func TestSanitizeProviderFileNamePartRejectsPathSeparators(t *testing.T) {
	for _, input := range []string{"../../outside", `..\..\outside`, "a/b", `a\b`, "..", ""} {
		got := sanitizeProviderFileNamePart(input)
		if strings.ContainsAny(got, `/\`) {
			t.Fatalf("sanitizeProviderFileNamePart(%q) = %q contains a path separator", input, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Fatalf("sanitizeProviderFileNamePart(%q) = %q must not start with a dot", input, got)
		}
		if filepath.Base(got) != got {
			t.Fatalf("sanitizeProviderFileNamePart(%q) = %q is not a base name", input, got)
		}
		if got == "" {
			t.Fatalf("sanitizeProviderFileNamePart(%q) returned an empty name", input)
		}
	}
}

// TestJoyCodeAuthKeyMatches verifies the callback state check binds the
// callback to the login attempt and rejects empty values.
func TestJoyCodeAuthKeyMatches(t *testing.T) {
	if !joyCodeAuthKeyMatches("abc123", "abc123") {
		t.Fatal("expected matching auth keys to be accepted")
	}
	if joyCodeAuthKeyMatches("abc123", "other") {
		t.Fatal("mismatched auth keys must be rejected")
	}
	if joyCodeAuthKeyMatches("", "") {
		t.Fatal("empty auth keys must be rejected")
	}
	if joyCodeAuthKeyMatches("abc123", "") {
		t.Fatal("missing expected auth key must be rejected")
	}
}
