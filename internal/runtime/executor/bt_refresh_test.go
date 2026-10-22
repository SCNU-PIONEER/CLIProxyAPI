package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	btauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/bt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type btRefreshLoginCall struct {
	phone    string
	password string
	proxy    string
}

// stubBTRefreshLogin replaces the credential acquisition seam and records the
// exact config entry the executor used.
func stubBTRefreshLogin(t *testing.T, calls *[]btRefreshLoginCall, token *btauth.BTTokenStorage, err error) {
	t.Helper()
	previous := btRefreshLogin
	t.Cleanup(func() { btRefreshLogin = previous })
	btRefreshLogin = func(_ context.Context, phone, password, proxy string) (*btauth.BTTokenStorage, error) {
		*calls = append(*calls, btRefreshLoginCall{phone: phone, password: password, proxy: proxy})
		if err != nil {
			return nil, err
		}
		return token, nil
	}
}

func newBTRefreshAuth(phone, proxyURL, prefix string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "bt-config-1",
		Provider: "bt",
		Label:    phone,
		Prefix:   prefix,
		ProxyURL: proxyURL,
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"source":   "config:bt[token]",
			"bt_phone": phone,
			"priority": "3",
		},
		Metadata: map[string]any{
			"type":       "bt",
			"bt_phone":   phone,
			"uid":        "old-uid",
			"access_key": "old-key",
			"serverid":   "old-server",
		},
	}
}

func TestBTRefreshUsesExactConfigIdentity(t *testing.T) {
	cfg := &config.Config{BTKey: []config.BTKey{
		{Phone: "13800000001", Password: "Zmlyc3Q=", Prefix: "a/"},
		{Phone: "13800000001", Password: "c2Vjb25k", Prefix: "b/", ProxyURL: "http://proxy-b:8080"},
	}}
	auth := newBTRefreshAuth("13800000001", "http://proxy-b:8080", "b/")
	var calls []btRefreshLoginCall
	stubBTRefreshLogin(t, &calls, &btauth.BTTokenStorage{UID: "new-uid", AccessKey: "new-key", ServerID: "new-server"}, nil)

	updated, err := (&BTExecutor{cfg: cfg}).Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("login calls = %d, want 1", len(calls))
	}
	if calls[0].password != "c2Vjb25k" || calls[0].proxy != "http://proxy-b:8080" {
		t.Fatalf("resolved entry = %+v, want the b/ entry", calls[0])
	}
	if got := metaStringValue(updated.Metadata, "uid"); got != "new-uid" {
		t.Fatalf("uid = %q, want new-uid", got)
	}
	if got := metaStringValue(updated.Metadata, "access_key"); got != "new-key" {
		t.Fatalf("access_key = %q, want new-key", got)
	}
	if got := metaStringValue(updated.Metadata, "serverid"); got != "new-server" {
		t.Fatalf("serverid = %q, want new-server", got)
	}
}

func TestBTRefreshRejectsMissingConfigEntry(t *testing.T) {
	cfg := &config.Config{BTKey: []config.BTKey{{Phone: "13800000001", Password: "cGFzcw=="}}}
	auth := newBTRefreshAuth("13900000000", "", "")
	var calls []btRefreshLoginCall
	stubBTRefreshLogin(t, &calls, &btauth.BTTokenStorage{UID: "new-uid"}, nil)

	updated, err := (&BTExecutor{cfg: cfg}).Refresh(context.Background(), auth)
	if err == nil {
		t.Fatal("refresh succeeded without a matching config entry")
	}
	if !strings.Contains(err.Error(), "refusing to refresh") {
		t.Fatalf("error = %q, want an explicit refusal", err)
	}
	if updated != nil {
		t.Fatalf("updated auth = %+v, want nil", updated)
	}
	if len(calls) != 0 {
		t.Fatalf("login calls = %d, want 0", len(calls))
	}
}

func TestBTRefreshRejectsAmbiguousConfigEntry(t *testing.T) {
	entry := config.BTKey{Phone: "13800000001", Password: "cGFzcw==", Prefix: "x/"}
	cfg := &config.Config{BTKey: []config.BTKey{entry, entry}}
	auth := newBTRefreshAuth("13800000001", "", "x/")
	var calls []btRefreshLoginCall
	stubBTRefreshLogin(t, &calls, &btauth.BTTokenStorage{UID: "new-uid"}, nil)

	_, err := (&BTExecutor{cfg: cfg}).Refresh(context.Background(), auth)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want an ambiguous-entry refusal", err)
	}
	if len(calls) != 0 {
		t.Fatalf("login calls = %d, want 0", len(calls))
	}
}

func TestBTRefreshNeverPersistsPassword(t *testing.T) {
	const password = "c2VjcmV0LXBhc3N3b3Jk"
	cfg := &config.Config{BTKey: []config.BTKey{{Phone: "13800000001", Password: password}}}
	auth := newBTRefreshAuth("13800000001", "", "")
	var calls []btRefreshLoginCall
	stubBTRefreshLogin(t, &calls, &btauth.BTTokenStorage{UID: "new-uid", AccessKey: "new-key", ServerID: "new-server"}, nil)

	updated, err := (&BTExecutor{cfg: cfg}).Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	raw, errMarshal := json.Marshal(updated)
	if errMarshal != nil {
		t.Fatalf("marshal updated auth: %v", errMarshal)
	}
	if strings.Contains(string(raw), password) {
		t.Fatalf("password leaked into refreshed auth: %s", raw)
	}
	if _, ok := updated.Metadata["password"]; ok {
		t.Fatal("password key present on refreshed auth metadata")
	}
	if got := auth.Metadata["uid"]; got != "old-uid" {
		t.Fatalf("original auth mutated: uid = %v", got)
	}
}

func TestBTRefreshPropagatesLoginFailure(t *testing.T) {
	cfg := &config.Config{BTKey: []config.BTKey{{Phone: "13800000001", Password: "cGFzcw=="}}}
	auth := newBTRefreshAuth("13800000001", "", "")
	var calls []btRefreshLoginCall
	stubBTRefreshLogin(t, &calls, nil, errors.New("boom"))

	updated, err := (&BTExecutor{cfg: cfg}).Refresh(context.Background(), auth)
	if err == nil || !strings.Contains(err.Error(), "refresh login failed") {
		t.Fatalf("error = %v, want refresh login failure", err)
	}
	if updated != nil {
		t.Fatalf("updated auth = %+v, want nil", updated)
	}
}
