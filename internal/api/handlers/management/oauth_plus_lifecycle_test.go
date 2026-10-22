package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func startPlusOAuthRawRequest(t *testing.T, h *Handler, target string) (*httptest.ResponseRecorder, map[string]any) {
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
	return recorder, payload
}

// TestPlusOAuthCommitFailureUnsticksSession covers the defect where a failed
// credential write left the session committing forever (timer stopped,
// cancellation refused).
func TestPlusOAuthCommitFailureUnsticksSession(t *testing.T) {
	h, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			return newFakePlusAuth("commit-fail.json"), nil
		},
	})
	h.postAuthHook = func(context.Context, *coreauth.Auth) error {
		return errors.New("secret-marker-save-xyz")
	}

	start := startPlusSessionRequest(t, h, "codebuddy")
	state, _ := start["state"].(string)
	status := waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "error"
	})
	if status["error"] != plusOAuthErrorSaveFailed {
		t.Fatalf("unexpected public save error: %v", status)
	}

	cancelled, committing, known := h.plusOAuthStore().cancel(state)
	if !known || committing || cancelled {
		t.Fatalf("failed commit must be terminal, not committing: known=%v committing=%v cancelled=%v", known, committing, cancelled)
	}
}

// TestPlusOAuthTerminalRetirementAndBudget covers the defect where finished
// sessions were never retired, permanently consuming the concurrency budget.
func TestPlusOAuthTerminalRetirementAndBudget(t *testing.T) {
	var timerMu sync.Mutex
	var retireFuncs []func()
	var expireFuncs []func()
	afterFunc := func(delay time.Duration, callback func()) *time.Timer {
		timerMu.Lock()
		defer timerMu.Unlock()
		if delay == plusOAuthCompletedKeep {
			retireFuncs = append(retireFuncs, callback)
		} else {
			expireFuncs = append(expireFuncs, callback)
		}
		return nil
	}

	store := newPlusOAuthSessionStoreWithAfterFunc(afterFunc)
	states := make([]string, 0, 20)
	for index := 0; index < 10; index++ {
		session, errCreate := store.createSession("qoder", context.Background())
		if errCreate != nil {
			t.Fatalf("create session %d: %v", index, errCreate)
		}
		if !session.beginCommit() {
			t.Fatalf("beginCommit session %d failed", index)
		}
		session.complete()
		states = append(states, session.state)
	}
	for index := 0; index < 10; index++ {
		session, errCreate := store.createSession("qoder", context.Background())
		if errCreate != nil {
			t.Fatalf("create failed session %d: %v", index, errCreate)
		}
		session.fail(plusOAuthErrorAuthFailed)
		states = append(states, session.state)
	}

	// All finished sessions are retained but must not consume the budget.
	extra, errCreate := store.createSession("qoder", context.Background())
	if errCreate != nil {
		t.Fatalf("terminal sessions must not consume the concurrency budget: %v", errCreate)
	}
	states = append(states, extra.state)

	timerMu.Lock()
	retireCount := len(retireFuncs)
	expireCount := len(expireFuncs)
	scheduled := append([]func(){}, retireFuncs...)
	timerMu.Unlock()
	if retireCount != 20 {
		t.Fatalf("retirement timers = %d, want 20", retireCount)
	}
	if expireCount != len(states) {
		t.Fatalf("expiry timers = %d, want %d (one fixed deadline per session)", expireCount, len(states))
	}

	for _, callback := range scheduled {
		callback()
	}
	for _, state := range states {
		if _, ok := store.get(state); ok && state != extra.state {
			t.Fatalf("session %s was not retired", state)
		}
	}
}

// TestPlusOAuthErrorsDoNotLeakSecrets covers the defect where provider and
// save error text was forwarded to clients and logs.
func TestPlusOAuthErrorsDoNotLeakSecrets(t *testing.T) {
	logBuffer := &lockedLogBuffer{}
	previousOut := log.StandardLogger().Out
	log.SetOutput(logBuffer)
	defer log.SetOutput(previousOut)

	h, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			return nil, errors.New("secret-marker-login-abc123")
		},
	})
	recorder, start := startPlusOAuthRawRequest(t, h, "/v8/management/oauth/auth-url?provider=codebuddy")
	state, _ := start["state"].(string)
	status := waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "error"
	})
	if status["error"] != plusOAuthErrorAuthFailed {
		t.Fatalf("login error must be a stable category, got %v", status)
	}
	if strings.Contains(recorder.Body.String(), "secret-marker") {
		t.Fatalf("start response leaked provider error text: %s", recorder.Body.String())
	}
	if strings.Contains(logBuffer.String(), "secret-marker") {
		t.Fatalf("logs leaked provider error text: %s", logBuffer.String())
	}

	// A save failure must also map to its own stable category without logging
	// the raw error value.
	h2, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			return newFakePlusAuth("save-leak.json"), nil
		},
	})
	h2.postAuthHook = func(context.Context, *coreauth.Auth) error {
		return errors.New("secret-marker-save-def456")
	}
	recorder2, start2 := startPlusOAuthRawRequest(t, h2, "/v8/management/oauth/auth-url?provider=codebuddy")
	state2, _ := start2["state"].(string)
	status2 := waitPlusStatus(t, h2.plusOAuthStore(), state2, func(view map[string]any) bool {
		return view["status"] == "error"
	})
	if status2["error"] != plusOAuthErrorSaveFailed {
		t.Fatalf("save error must be a stable category, got %v", status2)
	}
	if strings.Contains(recorder2.Body.String(), "secret-marker") {
		t.Fatalf("start response leaked save error text: %s", recorder2.Body.String())
	}
	if strings.Contains(logBuffer.String(), "secret-marker") {
		t.Fatalf("logs leaked save error text: %s", logBuffer.String())
	}
}

// TestPlusOAuthFixedTTLNotExtended covers the defect where the session expiry
// deadline was updated but neither the timer nor the deadline was enforced.
func TestPlusOAuthFixedTTLNotExtended(t *testing.T) {
	var timerMu sync.Mutex
	expireCount := 0
	afterFunc := func(delay time.Duration, callback func()) *time.Timer {
		timerMu.Lock()
		defer timerMu.Unlock()
		if delay != plusOAuthCompletedKeep {
			expireCount++
		}
		return nil
	}

	store := newPlusOAuthSessionStoreWithAfterFunc(afterFunc)
	session, errCreate := store.createSession("kilo", context.Background())
	if errCreate != nil {
		t.Fatalf("create session: %v", errCreate)
	}
	session.setChallenge(sdkAuth.LoginChallenge{URL: "https://example.test/device", UserCode: "CODE"})
	timerMu.Lock()
	count := expireCount
	timerMu.Unlock()
	if count != 1 {
		t.Fatalf("challenge activity must not extend the fixed TTL: expiry timers=%d", count)
	}
	payload := session.payload()
	if payload["status"] != plusOAuthStatusChallenge {
		t.Fatalf("unexpected challenge status: %v", payload)
	}
}

// TestPlusOAuthLoginMetadataWhitelist covers the defect where non-secret
// provider parameters were dropped and secret ones could arrive via query.
func TestPlusOAuthLoginMetadataWhitelist(t *testing.T) {
	optsCh := make(chan *sdkAuth.LoginOptions, 1)
	h, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "gitlab",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			optsCh <- opts
			return newFakePlusAuth("gitlab-meta.json"), nil
		},
	})
	recorder, start := startPlusOAuthRawRequest(t, h, "/v8/management/oauth/auth-url?provider=gitlab&base_url=https%3A%2F%2Fgitlab.example.com&client_id=client-abc&login_mode=pat&personal_access_token=secret-marker-token&oauth_client_secret=secret-marker-secret")
	state, _ := start["state"].(string)
	if state == "" {
		t.Fatalf("start response missing state: %v", start)
	}

	var opts *sdkAuth.LoginOptions
	select {
	case opts = <-optsCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("login options were not observed")
	}
	if opts.Metadata["base_url"] != "https://gitlab.example.com" {
		t.Fatalf("base_url not forwarded: %v", opts.Metadata)
	}
	if opts.Metadata["oauth_client_id"] != "client-abc" {
		t.Fatalf("client_id not mapped to oauth_client_id: %v", opts.Metadata)
	}
	if opts.Metadata["login_mode"] != "pat" {
		t.Fatalf("login_mode not forwarded: %v", opts.Metadata)
	}
	for key, value := range opts.Metadata {
		if strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(value, "secret-marker") {
			t.Fatalf("secret metadata forwarded from query: %s=%s", key, value)
		}
	}
	if strings.Contains(recorder.Body.String(), "secret-marker") {
		t.Fatalf("start response leaked secrets: %s", recorder.Body.String())
	}
	// The login worker persists the auth file asynchronously. Wait for its
	// terminal commit before t.TempDir cleanup unlinks the auth directory.
	waitPlusStatus(t, h.plusOAuthStore(), state, func(view map[string]any) bool {
		return view["status"] == "ok"
	})
}

// TestPlusOAuthHandlersAreIsolated covers the requirement that sessions cannot
// be observed or cancelled through another handler instance.
func TestPlusOAuthHandlersAreIsolated(t *testing.T) {
	hA, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			return newFakePlusAuth("isolation-a.json"), nil
		},
	})
	hB, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			return newFakePlusAuth("isolation-b.json"), nil
		},
	})

	start := startPlusSessionRequest(t, hA, "codebuddy")
	state, _ := start["state"].(string)
	waitPlusStatus(t, hA.plusOAuthStore(), state, func(view map[string]any) bool { return view["status"] == "ok" })

	if _, ok := hB.plusOAuthStore().status(state); ok {
		t.Fatalf("handler B observed handler A's session")
	}
	if _, _, known := hB.plusOAuthStore().cancel(state); known {
		t.Fatalf("handler B cancelled handler A's session")
	}

	hA.ClosePlusOAuthSessions()
	closedRecorder, closedPayload := startPlusOAuthRawRequest(t, hA, "/v8/management/oauth/auth-url?provider=codebuddy")
	if closedRecorder.Code != http.StatusServiceUnavailable || closedPayload["error"] != "oauth coordinator is closed" {
		t.Fatalf("closed coordinator must refuse starts: code=%d body=%s", closedRecorder.Code, closedRecorder.Body.String())
	}
	if recorder, _ := startPlusOAuthRawRequest(t, hB, "/v8/management/oauth/auth-url?provider=codebuddy"); recorder.Code != http.StatusOK {
		t.Fatalf("handler B must stay usable: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPlusOAuthCloseAndCancelPending covers shutdown cancellation plus the
// temporary cancel-pending path that keeps a coordinator reusable.
func TestPlusOAuthCloseAndCancelPending(t *testing.T) {
	started := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	h, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			started <- struct{}{}
			<-ctx.Done()
			done <- struct{}{}
			return nil, ctx.Err()
		},
	})
	start := startPlusSessionRequest(t, h, "codebuddy")
	state, _ := start["state"].(string)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("login did not start")
	}

	h.ClosePlusOAuthSessions()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("close did not cancel the pending login")
	}
	if payload, ok := h.plusOAuthStore().status(state); ok {
		t.Fatalf("closed coordinator still serves sessions: %v", payload)
	}

	started2 := make(chan struct{}, 1)
	h2, _ := newPlusOAuthTestHandler(t, &fakePlusAuthenticator{
		provider: "codebuddy",
		login: func(ctx context.Context, cfg *config.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
			started2 <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	start2 := startPlusSessionRequest(t, h2, "codebuddy")
	select {
	case <-started2:
	case <-time.After(2 * time.Second):
		t.Fatalf("second login did not start")
	}
	if cancelled := h2.CancelPendingPlusOAuthSessions(); cancelled != 1 {
		t.Fatalf("cancel pending = %d, want 1", cancelled)
	}
	if recorder, payload := startPlusOAuthRawRequest(t, h2, "/v8/management/oauth/auth-url?provider=codebuddy"); recorder.Code != http.StatusOK || payload["status"] != "wait" {
		t.Fatalf("coordinator must stay usable after cancel-pending: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	_ = start2
	h2.ClosePlusOAuthSessions()
}

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedLogBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedLogBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestPlusOAuthRetirementAfterCloseIsHarmless proves retention timers that
// fire after the coordinator closed only no-op.
func TestPlusOAuthRetirementAfterCloseIsHarmless(t *testing.T) {
	var timerMu sync.Mutex
	var retireFuncs []func()
	afterFunc := func(delay time.Duration, callback func()) *time.Timer {
		timerMu.Lock()
		defer timerMu.Unlock()
		if delay == plusOAuthCompletedKeep {
			retireFuncs = append(retireFuncs, callback)
		}
		return nil
	}

	store := newPlusOAuthSessionStoreWithAfterFunc(afterFunc)
	session, errCreate := store.createSession("qoder", context.Background())
	if errCreate != nil {
		t.Fatalf("create session: %v", errCreate)
	}
	if !session.beginCommit() {
		t.Fatalf("beginCommit failed")
	}
	session.complete()
	store.close()

	timerMu.Lock()
	scheduled := append([]func(){}, retireFuncs...)
	timerMu.Unlock()
	if len(scheduled) != 1 {
		t.Fatalf("retirement timers = %d, want 1", len(scheduled))
	}
	scheduled[0]()
	if _, ok := store.get(session.state); ok {
		t.Fatalf("closed store resurrected a retired session")
	}
}
