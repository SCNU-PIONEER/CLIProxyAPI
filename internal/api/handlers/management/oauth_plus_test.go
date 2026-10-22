package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakePlusAuthenticator struct {
	provider string
	login    func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error)
}

func (f *fakePlusAuthenticator) Provider() string { return f.provider }

func (f *fakePlusAuthenticator) RefreshLead() *time.Duration { return nil }

func (f *fakePlusAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
	return f.login(ctx, cfg, opts)
}

func newPlusOAuthTestHandler(t *testing.T, authenticators ...sdkAuth.Authenticator) (*Handler, string) {
	t.Helper()
	authDir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: authDir}, "", nil)
	manager := sdkAuth.NewManager(nil)
	for _, authenticator := range authenticators {
		manager.Register(authenticator)
	}
	h.SetSDKAuthManager(manager)
	return h, authDir
}

func newFakePlusAuth(fileName string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       "plus-test-" + fileName,
		Provider: "codebuddy",
		FileName: fileName,
		Label:    "plus test",
		Metadata: map[string]any{"type": "codebuddy"},
	}
}

func startPlusSessionRequest(t *testing.T, h *Handler, provider string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider="+provider, nil)
	if !h.StartPlusOAuthV8(ctx) {
		t.Fatalf("provider %q was not handled", provider)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode start response: %v", errDecode)
	}
	return payload
}

func startPlusSessionRequestWithQuery(t *testing.T, h *Handler, target string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	if !h.StartPlusOAuthV8(ctx) {
		t.Fatalf("request %q was not handled", target)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode start response: %v", errDecode)
	}
	return payload
}

func waitPlusStatus(t *testing.T, store *plusOAuthSessionStore, state string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if payload, ok := store.status(state); ok {
			view := map[string]any(payload)
			if match(view) {
				return view
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	payload, _ := store.status(state)
	t.Fatalf("status did not reach expectation, last=%v", payload)
	return nil
}

func TestPlusOAuthProviderCatalogNormalizesEndpoints(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: authDir}, "", nil)
	h.SetSDKAuthManager(sdkAuth.NewDefaultManager(nil))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/providers", nil)
	h.GetOAuthProvidersV8(ctx)

	var payload struct {
		Providers []struct {
			Key               string `json:"key"`
			AuthURLEndpoint   string `json:"auth_url_endpoint"`
			Available         bool   `json:"available"`
			UnavailableReason string `json:"unavailable_reason"`
			Flow              string `json:"flow"`
		} `json:"providers"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode providers: %v", errDecode)
	}
	seen := map[string]bool{}
	for _, provider := range payload.Providers {
		seen[provider.Key] = true
		if !strings.HasPrefix(provider.AuthURLEndpoint, "/v8/management/oauth/auth-url?provider=") {
			t.Fatalf("provider %q endpoint not normalized: %q", provider.Key, provider.AuthURLEndpoint)
		}
		if provider.Key == "kiro" {
			if !provider.Available {
				t.Fatalf("kiro web login should be available")
			}
			if provider.Flow != "device" {
				t.Fatalf("kiro flow=%q want device", provider.Flow)
			}
		}
	}
	for _, key := range []string{"codebuddy", "cursor", "gitlab", "kilo"} {
		if !seen[key] {
			t.Fatalf("provider catalog missing %q", key)
		}
	}
}

func TestStartPlusOAuthV8KiroRejectsUnsupportedMethod(t *testing.T) {
	h, _ := newPlusOAuthTestHandler(t, sdkAuth.NewKiroAuthenticator())

	start := startPlusSessionRequestWithQuery(t, h, "/v8/management/oauth/auth-url?provider=kiro&method=bogus")
	state, _ := start["state"].(string)
	if state == "" {
		t.Fatalf("start response missing state: %v", start)
	}
	status := waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "error"
	})
	errorText, _ := status["error"].(string)
	if errorText != plusOAuthErrorAuthFailed {
		t.Fatalf("provider error text must be a stable category, got %q", errorText)
	}
}

func TestPlusOAuthChallengeStatusAndCancel(t *testing.T) {
	challengeSent := make(chan struct{})
	h, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			if opts.OnChallenge == nil {
				return nil, errors.New("missing OnChallenge")
			}
			opts.OnChallenge(sdkAuth.LoginChallenge{URL: "https://example.test/device", UserCode: "ABCD-EFGH", VerificationURI: "https://example.test/verify"})
			close(challengeSent)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	start := startPlusSessionRequest(t, h, "codebuddy")
	state, _ := start["state"].(string)
	if state == "" {
		t.Fatalf("start response missing state: %v", start)
	}
	select {
	case <-challengeSent:
	case <-time.After(2 * time.Second):
		t.Fatalf("challenge was not delivered")
	}
	status := waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "challenge"
	})
	if status["url"] != "https://example.test/device" || status["user_code"] != "ABCD-EFGH" {
		t.Fatalf("unexpected challenge payload: %v", status)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/v8/management/oauth/session?state="+state, nil)
	h.CancelAuthSession(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var cancelPayload map[string]any
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &cancelPayload); errDecode != nil {
		t.Fatalf("decode cancel: %v", errDecode)
	}
	if cancelPayload["cancelled"] != true {
		t.Fatalf("cancel not acknowledged: %v", cancelPayload)
	}
	waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "error"
	})
}

func TestStartPlusOAuthV8CodeArtsDeliversRemoteChallenge(t *testing.T) {
	h, _ := newPlusOAuthTestHandler(t, sdkAuth.NewCodeArtsAuthenticator())

	start := startPlusSessionRequest(t, h, "codearts")
	state, _ := start["state"].(string)
	if state == "" {
		t.Fatalf("start response missing state: %v", start)
	}
	status := waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "input" && view["url"] != nil
	})
	challengeURL, _ := status["url"].(string)
	if challengeURL == "" {
		t.Fatalf("codearts challenge URL missing: %v", status)
	}
	if !strings.HasPrefix(challengeURL, "https://") {
		t.Fatalf("unexpected codearts challenge URL: %q", challengeURL)
	}

	cancelled, committing, known := h.plusOAuthStore().cancel(state)
	if !known || committing || !cancelled {
		t.Fatalf("cancel codearts session: known=%v committing=%v cancelled=%v", known, committing, cancelled)
	}
	waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "error"
	})
}

func TestStartPlusOAuthV8JoyCodeDeliversRemoteChallenge(t *testing.T) {
	h, _ := newPlusOAuthTestHandler(t, sdkAuth.NewJoyCodeAuthenticator())

	start := startPlusSessionRequest(t, h, "joycode")
	state, _ := start["state"].(string)
	if state == "" {
		t.Fatalf("start response missing state: %v", start)
	}
	status := waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "input" && view["url"] != nil
	})
	challengeURL, _ := status["url"].(string)
	if !strings.Contains(challengeURL, "joycode.jd.com") {
		t.Fatalf("unexpected joycode challenge URL: %q", challengeURL)
	}
	if _, _, known := h.plusOAuthStore().cancel(state); !known {
		t.Fatalf("joycode session should exist")
	}
}

func TestPlusOAuthPromptInputPersistsRecord(t *testing.T) {
	h, authDir := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			if opts.Prompt == nil {
				return nil, errors.New("missing prompt")
			}
			value, errPrompt := opts.Prompt("Paste the provider callback URL")
			if errPrompt != nil {
				return nil, errPrompt
			}
			if value != "https://provider.test/callback?code=code-1" {
				return nil, errors.New("unexpected prompt value")
			}
			return newFakePlusAuth("plus-input.json"), nil
		},
	})

	start := startPlusSessionRequest(t, h, "codebuddy")
	state, _ := start["state"].(string)
	status := waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "input"
	})
	promptID, _ := status["prompt_id"].(string)
	if promptID == "" {
		t.Fatalf("prompt_id missing: %v", status)
	}
	if status["prompt"] != "Paste the provider callback URL" {
		t.Fatalf("unexpected prompt text: %v", status)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	body := `{"state":"` + state + `","prompt_id":"` + promptID + `","input":"https://provider.test/callback?code=code-1"}`
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/oauth/session/input", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PostOAuthSessionInput(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("input status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "ok"
	})
	if _, errStat := os.Stat(filepath.Join(authDir, "plus-input.json")); errStat != nil {
		t.Fatalf("credential file was not persisted: %v", errStat)
	}
}

func TestPlusOAuthCommitFenceRejectsCancel(t *testing.T) {
	st := newPlusOAuthSessionStore()
	session, errCreate := st.createSession("codebuddy", context.Background())
	if errCreate != nil {
		t.Fatalf("create session: %v", errCreate)
	}
	if !session.beginCommit() {
		t.Fatalf("beginCommit failed")
	}
	cancelled, committing, known := st.cancel(session.state)
	if !known || !committing {
		t.Fatalf("cancel during commit: known=%v committing=%v", known, committing)
	}
	if cancelled {
		t.Fatalf("cancelling a committing session must not be acknowledged")
	}
	session.complete()
	payload := waitPlusStatus(t, st, session.state, func(view map[string]any) bool {
		return view["status"] == "ok"
	})
	if payload["status"] != "ok" {
		t.Fatalf("unexpected committed status: %v", payload)
	}
}

func TestPlusOAuthPromptRejectsStaleAndUnknown(t *testing.T) {
	st := newPlusOAuthSessionStore()
	session, errCreate := st.createSession("gitlab", context.Background())
	if errCreate != nil {
		t.Fatalf("create session: %v", errCreate)
	}
	valueCh := make(chan string, 1)
	go func() {
		value, _ := session.requestInput(context.Background(), "Paste the auth code")
		valueCh <- value
	}()
	status := waitPlusStatus(t, st, session.state, func(view map[string]any) bool {
		return view["status"] == "input"
	})
	promptID, _ := status["prompt_id"].(string)

	if errStale := st.deliverInput(session.state, "wrong-prompt-id", "value"); !errors.Is(errStale, errPlusOAuthStalePrompt) {
		t.Fatalf("expected stale prompt error, got %v", errStale)
	}
	if errUnknown := st.deliverInput("does-not-exist", promptID, "value"); !errors.Is(errUnknown, errPlusOAuthUnknownSession) {
		t.Fatalf("expected unknown session error, got %v", errUnknown)
	}
	if errDeliver := st.deliverInput(session.state, promptID, "auth-code-1"); errDeliver != nil {
		t.Fatalf("deliver input: %v", errDeliver)
	}
	select {
	case value := <-valueCh:
		if value != "auth-code-1" {
			t.Fatalf("unexpected value: %q", value)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("prompt value was not delivered")
	}
}

func TestPlusOAuthSessionExpiryCleansUp(t *testing.T) {
	st := newPlusOAuthSessionStore()
	session, errCreate := st.createSession("iflow", context.Background())
	if errCreate != nil {
		t.Fatalf("create session: %v", errCreate)
	}
	st.expire(session.state)
	payload := waitPlusStatus(t, st, session.state, func(view map[string]any) bool {
		return view["status"] == "error"
	})
	if payload["error"] != "oauth session expired" {
		t.Fatalf("unexpected expiry payload: %v", payload)
	}
	select {
	case <-session.loginCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("expired session context was not cancelled")
	}
}

func TestPlusOAuthSessionLimit(t *testing.T) {
	st := newPlusOAuthSessionStore()
	for index := 0; index < plusOAuthMaxSessions; index++ {
		if _, errCreate := st.createSession("qoder", context.Background()); errCreate != nil {
			t.Fatalf("create session %d: %v", index, errCreate)
		}
	}
	if _, errCreate := st.createSession("qoder", context.Background()); !errors.Is(errCreate, errPlusOAuthTooManySessions) {
		t.Fatalf("expected session limit error, got %v", errCreate)
	}
}

func TestPostOAuthSessionInputRejectsInvalidState(t *testing.T) {
	h, _ := newPlusOAuthTestHandler(t)

	badRecorder := httptest.NewRecorder()
	badCtx, _ := gin.CreateTestContext(badRecorder)
	badCtx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/oauth/session/input", strings.NewReader(`{"state":"../evil","input":"x"}`))
	badCtx.Request.Header.Set("Content-Type", "application/json")
	h.PostOAuthSessionInput(badCtx)
	if badRecorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed state status=%d body=%s", badRecorder.Code, badRecorder.Body.String())
	}

	unknownRecorder := httptest.NewRecorder()
	unknownCtx, _ := gin.CreateTestContext(unknownRecorder)
	unknownCtx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/oauth/session/input", strings.NewReader(`{"state":"0123456789abcdef0123456789abcdef","input":"x"}`))
	unknownCtx.Request.Header.Set("Content-Type", "application/json")
	h.PostOAuthSessionInput(unknownCtx)
	if unknownRecorder.Code != http.StatusNotFound {
		t.Fatalf("unknown state status=%d body=%s", unknownRecorder.Code, unknownRecorder.Body.String())
	}
}

func TestStartPlusOAuthV8PassesNoBrowserForRemoteClients(t *testing.T) {
	optsCh := make(chan *sdkAuth.LoginOptions, 1)
	h, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			optsCh <- opts
			return newFakePlusAuth("nobrowser.json"), nil
		},
	})
	start := startPlusSessionRequest(t, h, "codebuddy")
	state, _ := start["state"].(string)
	waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool { return view["status"] == "ok" })
	select {
	case opts := <-optsCh:
		if opts == nil || !opts.NoBrowser {
			t.Fatalf("remote clients must run logins with NoBrowser=true: %+v", opts)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("login options were not observed")
	}
}
