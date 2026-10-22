package auth

import "strings"

// IsConfigAPIKeyAuth reports whether the auth entry is synthesized from config *-api-key lists.
func IsConfigAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.AuthKind() != AuthKindAPIKey {
		return false
	}
	return auth.AuthSourceKind() == AuthSourceConfig
}

// IsConfigManagedOAuthAuth reports whether the auth entry is synthesized from
// the config file (Kiro/BaoTa entries). Those credentials are re-synthesized on
// every config reload, so writing refreshed runtime tokens back into the auth
// store would create path-less orphan files and let runtime token material
// shadow the config source of truth.
func IsConfigManagedOAuthAuth(auth *Auth) bool {
	if auth == nil || auth.AuthSourceKind() != AuthSourceConfig || auth.Attributes == nil {
		return false
	}
	source := strings.ToLower(strings.TrimSpace(auth.Attributes["source"]))
	return strings.HasPrefix(source, "config:kiro[") || strings.HasPrefix(source, "config:bt[")
}
