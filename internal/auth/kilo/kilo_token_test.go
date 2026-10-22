package kilo

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCredentialFileNameRejectsPathSeparators ensures the provider-supplied
// email cannot escape the auth directory when used as a credential file name.
func TestCredentialFileNameRejectsPathSeparators(t *testing.T) {
	inputs := []string{
		"../../outside",
		`..\..\outside`,
		"a/b@example.com",
		`a\b@example.com`,
		"..",
	}
	for _, input := range inputs {
		got := CredentialFileName(input)
		if strings.ContainsAny(got, `/\`) {
			t.Fatalf("CredentialFileName(%q) = %q contains a path separator", input, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Fatalf("CredentialFileName(%q) = %q must not start with a dot", input, got)
		}
		if filepath.Base(got) != got {
			t.Fatalf("CredentialFileName(%q) = %q is not a base name", input, got)
		}
		if !strings.HasSuffix(got, ".json") {
			t.Fatalf("CredentialFileName(%q) = %q missing .json suffix", input, got)
		}
	}
}

// TestCredentialFileNameKeepsSafeEmail ensures ordinary emails are preserved.
func TestCredentialFileNameKeepsSafeEmail(t *testing.T) {
	if got := CredentialFileName("user@example.com"); got != "kilo-user@example.com.json" {
		t.Fatalf("unexpected filename: %q", got)
	}
}
