package cmd

import (
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

// newAuthManager creates a new authentication manager instance with all supported
// authenticators and a file-based token store. The provider list is shared with
// the SDK service and management API via sdkAuth.NewDefaultManager.
//
// Returns:
//   - *sdkAuth.Manager: A configured authentication manager instance
func newAuthManager() *sdkAuth.Manager {
	return sdkAuth.NewDefaultManager(sdkAuth.GetTokenStore())
}
