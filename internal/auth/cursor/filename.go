package cursor

import (
	"fmt"
	"strings"
)

// CredentialFileName returns the filename used to persist Cursor credentials.
// Priority: explicit label > auto-generated from JWT sub hash.
// If both label and subHash are empty, falls back to "cursor.json".
func CredentialFileName(label, subHash string) string {
	label = sanitizeFileNamePart(label)
	subHash = sanitizeFileNamePart(subHash)
	if label != "" {
		return fmt.Sprintf("cursor.%s.json", label)
	}
	if subHash != "" {
		return fmt.Sprintf("cursor.%s.json", subHash)
	}
	return "cursor.json"
}

// DisplayLabel returns a human-readable label for the Cursor account.
func DisplayLabel(label, subHash string) string {
	label = strings.TrimSpace(label)
	if label != "" {
		return "Cursor " + label
	}
	if subHash != "" {
		return "Cursor " + subHash
	}
	return "Cursor User"
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
