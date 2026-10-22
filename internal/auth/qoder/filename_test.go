package qoder

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCredentialFileNameRejectsPathSeparators ensures the provider-supplied uid
// cannot escape the auth directory when used as a credential file name.
func TestCredentialFileNameRejectsPathSeparators(t *testing.T) {
	inputs := []string{
		"../../outside",
		`..\..\outside`,
		"a/b",
		`a\b`,
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

// TestCredentialFileNameKeepsSafeUID ensures ordinary uids are preserved.
func TestCredentialFileNameKeepsSafeUID(t *testing.T) {
	if got := CredentialFileName("user-123"); got != "qoder-user-123.json" {
		t.Fatalf("unexpected filename: %q", got)
	}
}
