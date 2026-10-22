package cliproxy

import (
	"testing"
	"time"

	btauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/bt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestBuildBTConfigAuthSetsPerAuthRefreshInterval pins the scheduling contract:
// config-managed BT auths carry no expiry, so activation stamps a per-auth 6h
// periodic interval instead of relying on a global provider lead.
func TestBuildBTConfigAuthSetsPerAuthRefreshInterval(t *testing.T) {
	auth := buildBTConfigAuth(config.BTKey{Phone: "13800000001"}, 0, &btauth.BTTokenStorage{UID: "uid-1", AccessKey: "key-1", ServerID: "server-1"})
	if auth == nil {
		t.Fatal("nil auth")
	}
	value, ok := auth.Metadata["refresh_interval_seconds"]
	if !ok {
		t.Fatal("config BT auth is missing refresh_interval_seconds")
	}
	seconds, ok := value.(int64)
	if !ok {
		t.Fatalf("refresh_interval_seconds type = %T, want int64", value)
	}
	if want := int64((6 * time.Hour) / time.Second); seconds != want {
		t.Fatalf("refresh_interval_seconds = %d, want %d", seconds, want)
	}
}
