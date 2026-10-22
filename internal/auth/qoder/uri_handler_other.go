//go:build !windows

package qoder

import "errors"

// ErrURIHandlerOptInRequired is returned when installing the qoder:// handler
// was not explicitly requested.
var ErrURIHandlerOptInRequired = errors.New("qoder: URI handler installation requires explicit opt-in")

// RegisterURIHandler is a no-op on non-Windows platforms.
// On Linux/macOS, the qoder:// protocol would need xdg-open or other platform-specific handling.
// For now, users on non-Windows platforms should paste the callback URL manually.
// The opt-in is still required so callers have the same contract on every platform.
func RegisterURIHandler(callbackPort int, callbackState string, allowInstall bool) (func(), error) {
	if !allowInstall {
		return nil, ErrURIHandlerOptInRequired
	}
	return func() {}, nil
}

// UnregisterURIHandler is a no-op on non-Windows platforms.
func UnregisterURIHandler() {}
