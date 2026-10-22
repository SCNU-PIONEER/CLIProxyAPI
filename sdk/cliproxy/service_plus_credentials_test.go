package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	btauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/bt"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBuildBTConfigAuthKeepsSecretsOutOfMetadata(t *testing.T) {
	entry := config.BTKey{
		Phone:    " 13800000000 ",
		Password: "super-secret-password",
		Prefix:   "team",
		ProxyURL: "http://127.0.0.1:7890",
		Priority: 3,
		Headers:  map[string]string{"X-Team": "alpha"},
	}
	token := &btauth.BTTokenStorage{UID: "42", AccessKey: "ak-123", ServerID: "sid-1", Type: "bt"}
	auth := buildBTConfigAuth(entry, 2, token)
	if auth.Provider != "bt" || auth.Prefix != "team" || auth.ProxyURL != entry.ProxyURL {
		t.Fatalf("unexpected auth header fields: %+v", auth)
	}
	if auth.Attributes["config_index"] != "2" || auth.Attributes["priority"] != "3" {
		t.Fatalf("attributes = %v", auth.Attributes)
	}
	if auth.Attributes["header:X-Team"] != "alpha" {
		t.Fatalf("header attribute missing: %v", auth.Attributes)
	}
	if auth.Metadata["uid"] != "42" || auth.Metadata["access_key"] != "ak-123" || auth.Metadata["serverid"] != "sid-1" {
		t.Fatalf("metadata = %v", auth.Metadata)
	}
	payload, errMarshal := json.Marshal(auth)
	if errMarshal != nil {
		t.Fatalf("marshal auth: %v", errMarshal)
	}
	for _, forbidden := range []string{"super-secret-password", entry.Password} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("auth payload leaks password material: %s", string(payload))
		}
	}
	if strings.Contains(auth.ID, "super-secret-password") {
		t.Fatalf("auth id leaks password material: %s", auth.ID)
	}
	id, _ := btConfigAuthIdentity(entry)
	if auth.ID != id {
		t.Fatalf("auth id = %q, want stable %q", auth.ID, id)
	}
}

func TestStartPlusCredentialActivationRegistersAndRemovesBT(t *testing.T) {
	originalLogin := plusBTLogin
	defer func() { plusBTLogin = originalLogin }()

	var mu sync.Mutex
	logins := 0
	plusBTLogin = func(ctx context.Context, phone, password, proxyURL string) (*btauth.BTTokenStorage, error) {
		mu.Lock()
		logins++
		mu.Unlock()
		if proxyURL != "http://proxy.local:8080" {
			t.Errorf("proxyURL = %q, want the configured proxy", proxyURL)
		}
		return &btauth.BTTokenStorage{UID: "uid-1", AccessKey: "ak-1", ServerID: "sid-1", Type: "bt"}, nil
	}

	cfg := &config.Config{BTKey: []config.BTKey{{
		Phone:    "13800000000",
		Password: "cGFzcw==",
		Prefix:   "bt",
		ProxyURL: "http://proxy.local:8080",
		Priority: 2,
	}}}
	entry := cfg.BTKey[0]
	id, _ := btConfigAuthIdentity(entry)

	svc := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.startPlusCredentialActivation(ctx, cfg)

	registered, ok := svc.waitForRegisteredPlusAuth(ctx, id, 3*time.Second)
	if !ok {
		t.Fatal("BT config credential was not registered")
	}
	if registered.Prefix != "bt" || registered.ProxyURL != entry.ProxyURL {
		t.Fatalf("registered auth = %+v", registered)
	}
	if registered.Metadata["uid"] != "uid-1" || registered.Metadata["access_key"] != "ak-1" {
		t.Fatalf("registered metadata = %v", registered.Metadata)
	}

	svc.startPlusCredentialActivation(ctx, &config.Config{})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, exists := svc.coreManager.GetByID(id); !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("removed BT config credential was not deleted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if logins != 1 {
		t.Fatalf("login calls = %d, want 1 (existing credential must be reused)", logins)
	}
}

func TestStartPlusCredentialActivationReusesBTTokensAcrossReload(t *testing.T) {
	originalLogin := plusBTLogin
	defer func() { plusBTLogin = originalLogin }()

	var mu sync.Mutex
	logins := 0
	plusBTLogin = func(ctx context.Context, phone, password, proxyURL string) (*btauth.BTTokenStorage, error) {
		mu.Lock()
		logins++
		mu.Unlock()
		return &btauth.BTTokenStorage{UID: "uid-1", AccessKey: "ak-1", ServerID: "sid-1", Type: "bt"}, nil
	}

	entry := config.BTKey{Phone: "13800000000", Password: "cGFzcw==", Prefix: "bt"}
	id, _ := btConfigAuthIdentity(entry)
	svc := &Service{cfg: &config.Config{BTKey: []config.BTKey{entry}}, coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.startPlusCredentialActivation(ctx, svc.cfg)
	if _, ok := svc.waitForRegisteredPlusAuth(ctx, id, 3*time.Second); !ok {
		t.Fatal("BT config credential was not registered")
	}

	// A presentation-only reload keeps the same credential id and must reuse the
	// already acquired tokens instead of logging in again.
	reloaded := entry
	reloaded.Priority = 5
	reloaded.Headers = map[string]string{"X-Team": "beta"}
	svc.cfg = &config.Config{BTKey: []config.BTKey{reloaded}}
	svc.startPlusCredentialActivation(ctx, svc.cfg)
	deadline := time.Now().Add(3 * time.Second)
	for {
		current, ok := svc.coreManager.GetByID(id)
		if ok && current.Attributes["priority"] == "5" && current.Attributes["header:X-Team"] == "beta" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reloaded presentation was not applied, auth = %+v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if logins != 1 {
		t.Fatalf("login calls = %d, want 1 (tokens reused across reload)", logins)
	}
}

func TestStartPlusCredentialActivationFencesStaleLogin(t *testing.T) {
	originalLogin := plusBTLogin
	defer func() { plusBTLogin = originalLogin }()

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	plusBTLogin = func(ctx context.Context, phone, password, proxyURL string) (*btauth.BTTokenStorage, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return &btauth.BTTokenStorage{UID: "uid-stale", AccessKey: "ak-stale", ServerID: "sid-stale", Type: "bt"}, nil
	}

	entry := config.BTKey{Phone: "13800000000", Password: "cGFzcw=="}
	id, _ := btConfigAuthIdentity(entry)
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{entry}})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("BT login did not start")
	}
	// The entry is removed while its login is still in flight.
	svc.startPlusCredentialActivation(ctx, &config.Config{})
	close(release)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, ok := svc.coreManager.GetByID(id); ok {
			t.Fatal("stale credential was registered after its config entry was removed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSynthesizeKiroConfigAuthsKeepsInlineEntry(t *testing.T) {
	cfg := &config.Config{KiroKey: []config.KiroKey{{
		AccessToken:       "access-token",
		RefreshToken:      "refresh-token",
		ProfileArn:        "arn:aws:codewhisperer:us-east-1:1:profile/x",
		Region:            "us-east-1",
		PreferredEndpoint: "codewhisperer",
	}}}
	auths := synthesizeKiroConfigAuths(cfg)
	if len(auths) != 1 {
		t.Fatalf("auths = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "kiro" || auth.AuthSourceKind() != coreauth.AuthSourceConfig {
		t.Fatalf("auth = %+v", auth)
	}
	if auth.Metadata["access_token"] != "access-token" || auth.Metadata["refresh_token"] != "refresh-token" {
		t.Fatalf("metadata = %v", auth.Metadata)
	}
	if auth.Attributes["preferred_endpoint"] != "codewhisperer" {
		t.Fatalf("attributes = %v", auth.Attributes)
	}
}

func TestConfigReloadActivatesAndRemovesBTViaApplyConfigUpdate(t *testing.T) {
	originalLogin := plusBTLogin
	defer func() { plusBTLogin = originalLogin }()
	plusBTLogin = func(ctx context.Context, phone, password, proxyURL string) (*btauth.BTTokenStorage, error) {
		return &btauth.BTTokenStorage{UID: "uid-reload", AccessKey: "ak-reload", ServerID: "sid-reload", Type: "bt"}, nil
	}

	entry := config.BTKey{Phone: "13800000000", Password: "cGFzcw==", Priority: 1}
	id, _ := btConfigAuthIdentity(entry)
	baseCfg := &config.Config{BTKey: []config.BTKey{entry}}
	svc := &Service{cfg: baseCfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !svc.applyConfigUpdateWithAuthSynthesis(ctx, baseCfg, true) {
		t.Fatal("initial config update was not applied")
	}
	if _, ok := svc.waitForRegisteredPlusAuth(ctx, id, 5*time.Second); !ok {
		t.Fatal("BT config credential was not activated by config update")
	}

	nextCfg := &config.Config{}
	if !svc.applyConfigUpdateWithAuthSynthesis(ctx, nextCfg, true) {
		t.Fatal("removal config update was not applied")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, exists := svc.coreManager.GetByID(id); !exists {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("BT config credential survived config removal")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartPlusCredentialActivationRefreshesKiroRefreshOnlyCredential(t *testing.T) {
	originalRefresh := plusKiroForceRefresh
	defer func() { plusKiroForceRefresh = originalRefresh }()
	refreshed := make(chan string, 1)
	plusKiroForceRefresh = func(ctx context.Context, s *Service, id string) (*coreauth.Auth, error) {
		select {
		case refreshed <- id:
		default:
		}
		return nil, errors.New("refresh token rejected by test")
	}

	entry := config.KiroKey{RefreshToken: "refresh-only", ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/test", Region: "us-east-1"}
	cfg := &config.Config{KiroKey: []config.KiroKey{entry}}
	svc := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.startPlusCredentialActivation(ctx, cfg)
	kro := synthesizeKiroConfigAuths(cfg)
	if len(kro) != 1 {
		t.Fatalf("expected one synthesized kiro auth, got %d", len(kro))
	}

	select {
	case id := <-refreshed:
		if id != kro[0].ID {
			t.Fatalf("refresh invoked for %q, want %q", id, kro[0].ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh-only kiro config credential was not refreshed")
	}
}

func TestConfigReloadUpdatesKiroFingerprintConfig(t *testing.T) {
	previousFingerprint := &config.Config{KiroFingerprint: nil}
	defer func() { kiroauth.InitFingerprintConfig(previousFingerprint) }()

	fingerprint := func() string {
		return kiroauth.GlobalFingerprintManager().GetFingerprint("reload-test").BuildUserAgent()
	}

	first := &config.Config{KiroFingerprint: &config.KiroFingerprintConfig{
		StreamingSDKVersion: "1.0.1",
		OSType:              "linux",
		OSVersion:           "6.1",
		NodeVersion:         "20.0.0",
		KiroVersion:         "0.1.0",
		KiroHash:            "hash-one",
	}}
	svc := &Service{cfg: first, coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !svc.applyConfigUpdateWithAuthSynthesis(ctx, first, true) {
		t.Fatal("initial config update was not applied")
	}
	if got := fingerprint(); !strings.Contains(got, "KiroIDE-0.1.0-hash-one") {
		t.Fatalf("initial fingerprint config not applied: %q", got)
	}

	second := &config.Config{KiroFingerprint: &config.KiroFingerprintConfig{
		StreamingSDKVersion: "2.0.2",
		OSType:              "windows",
		OSVersion:           "10.0",
		NodeVersion:         "22.0.0",
		KiroVersion:         "0.2.0",
		KiroHash:            "hash-two",
	}}
	if !svc.applyConfigUpdateWithAuthSynthesis(ctx, second, true) {
		t.Fatal("reload config update was not applied")
	}
	if got := fingerprint(); !strings.Contains(got, "KiroIDE-0.2.0-hash-two") || strings.Contains(got, "hash-one") {
		t.Fatalf("reloaded fingerprint config not applied: %q", got)
	}

	cleared := &config.Config{}
	if !svc.applyConfigUpdateWithAuthSynthesis(ctx, cleared, true) {
		t.Fatal("clearing config update was not applied")
	}
	if got := fingerprint(); strings.Contains(got, "hash-two") {
		t.Fatalf("cleared fingerprint config still applied: %q", got)
	}
}

func TestKiroTokenFileChangeReactivatesConfigCredential(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "kiro-token.json")
	writeToken := func(accessToken string) {
		t.Helper()
		body := `{"accessToken":"` + accessToken + `","refreshToken":"refresh-value","profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/test","region":"us-east-1"}`
		if errWrite := os.WriteFile(tokenPath, []byte(body), 0o600); errWrite != nil {
			t.Fatalf("write token file: %v", errWrite)
		}
	}
	writeToken("first-access")

	cfg := &config.Config{KiroKey: []config.KiroKey{{TokenFile: tokenPath}}}
	svc := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	synthesized := synthesizeKiroConfigAuths(cfg)
	if len(synthesized) != 1 {
		t.Fatalf("synthesized %d kiro auths, want 1", len(synthesized))
	}
	id := synthesized[0].ID

	svc.startPlusCredentialActivation(ctx, cfg)
	defer svc.stopPlusCredentialActivation()
	waitAccessToken := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			current, ok := svc.coreManager.GetByID(id)
			if ok && metaString(current.Metadata["access_token"]) == want {
				return
			}
			if time.Now().After(deadline) {
				if current == nil {
					t.Fatalf("kiro config credential never registered (want token %q)", want)
				}
				t.Fatalf("kiro config credential token = %q, want %q", metaString(current.Metadata["access_token"]), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitAccessToken("first-access")

	writeToken("second-access")

	// The new ID is only stable while the token file contents are identical, so
	// locate the re-synthesized credential instead of reusing the first ID.
	deadline := time.Now().Add(5 * time.Second)
	for {
		auths := synthesizeKiroConfigAuths(cfg)
		if len(auths) == 1 {
			if current, ok := svc.coreManager.GetByID(auths[0].ID); ok && metaString(current.Metadata["access_token"]) == "second-access" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("token file change was not applied without a config reload")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
