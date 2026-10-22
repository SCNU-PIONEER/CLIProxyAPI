package qoder

import (
	"fmt"
	"strings"
)

// CredentialFileName returns the filename used to persist Qoder credentials.
// It uses the uid as a suffix to disambiguate accounts.
func CredentialFileName(uid string) string {
	uid = sanitizeFileNamePart(uid)
	if uid == "" {
		return "qoder.json"
	}
	return fmt.Sprintf("qoder-%s.json", uid)
}

// sanitizeFileNamePart keeps a provider-supplied value usable as a single
// filename component: path separators and other unsafe characters are
// replaced, leading/trailing dots, underscores and dashes are trimmed, and the
// result is capped to keep file paths bounded.
func sanitizeFileNamePart(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == '@':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return ""
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
