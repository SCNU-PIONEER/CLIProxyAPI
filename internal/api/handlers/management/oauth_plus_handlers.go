package management

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	log "github.com/sirupsen/logrus"
)

const plusOAuthAuthURLPath = "/v8/management/oauth/auth-url"

// plusOAuthProviderSupport describes how the management API drives a Plus
// provider login. Available is false when the browser flow cannot complete
// remotely yet; the catalog reports the reason instead of faking support.
type plusOAuthProviderSupport struct {
	Flow      string
	Available bool
	Reason    string
}

var plusOAuthProviderCatalog = map[string]plusOAuthProviderSupport{
	"codearts":       {Flow: "callback", Available: true},
	"codebuddy":      {Flow: "device", Available: true},
	"codebuddy-ai":   {Flow: "device", Available: true},
	"cursor":         {Flow: "device", Available: true},
	"github-copilot": {Flow: "device", Available: true},
	"gitlab":         {Flow: "input", Available: true},
	"iflow":          {Flow: "input", Available: true},
	"joycode":        {Flow: "callback", Available: true},
	"kilo":           {Flow: "device", Available: true},
	"kiro":           {Flow: "device", Available: true},
	"qoder":          {Flow: "input", Available: true},
}

// plusOAuthStore returns the coordinator owned by this handler, creating it on
// first use for handlers constructed as a literal in tests.
func (h *Handler) plusOAuthStore() *plusOAuthSessionStore {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.plusOAuthSessions == nil {
		h.plusOAuthSessions = newPlusOAuthSessionStore()
	}
	return h.plusOAuthSessions
}

// ClosePlusOAuthSessions stops the handler's OAuth coordinator: pending logins
// are cancelled and future starts are refused. A credential write that is
// already committing may still finish; this call never rolls it back.
func (h *Handler) ClosePlusOAuthSessions() {
	if h == nil {
		return
	}
	h.mu.Lock()
	store := h.plusOAuthSessions
	h.mu.Unlock()
	if store != nil {
		store.close()
	}
}

// CancelPendingPlusOAuthSessions cancels logins that have not reached the
// commit fence while leaving the coordinator usable (config reload / temporary
// management disable).
func (h *Handler) CancelPendingPlusOAuthSessions() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	store := h.plusOAuthSessions
	h.mu.Unlock()
	if store == nil {
		return 0
	}
	return store.cancelPending()
}

// sdkAuthManagerSnapshot returns the configured SDK auth manager.
func (h *Handler) sdkAuthManagerSnapshot() *sdkAuth.Manager {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sdkAuthManager
}

// runtimeConfigSnapshot clones the current config for an async login flow.
func (h *Handler) runtimeConfigSnapshot() *config.Config {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return nil
	}
	return h.cfg.CloneForRuntime()
}

// plusOAuthProviderView is a provider catalog entry with management metadata.
type plusOAuthProviderView struct {
	sdkAuth.ProviderInfo
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	Flow              string `json:"flow,omitempty"`
}

// GetOAuthProvidersV8 lists providers and normalizes legacy endpoint metadata
// to the generic v8 auth-url route.
func (h *Handler) GetOAuthProvidersV8(c *gin.Context) {
	manager := h.sdkAuthManagerSnapshot()
	providers := make([]plusOAuthProviderView, 0)
	if manager != nil {
		for _, info := range manager.ListProviders() {
			info.AuthURLEndpoint = plusOAuthAuthURLPath + "?provider=" + url.QueryEscape(info.Key)
			view := plusOAuthProviderView{ProviderInfo: info, Available: true}
			if support, ok := plusOAuthProviderCatalog[strings.ToLower(strings.TrimSpace(info.Key))]; ok {
				view.Flow = support.Flow
				view.Available = support.Available
				view.UnavailableReason = support.Reason
			}
			providers = append(providers, view)
		}
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

// StartPlusOAuthV8 starts a Plus provider login. It returns false when the
// provider is not managed here so the caller can fall back to plugin flows.
func (h *Handler) StartPlusOAuthV8(c *gin.Context) bool {
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	support, ok := plusOAuthProviderCatalog[provider]
	if !ok {
		return false
	}
	if !support.Available {
		reason := strings.TrimSpace(support.Reason)
		if reason == "" {
			reason = "provider login is not available"
		}
		c.JSON(http.StatusNotImplemented, gin.H{"status": "error", "provider": provider, "error": reason})
		return true
	}

	manager := h.sdkAuthManagerSnapshot()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "provider": provider, "error": "auth manager unavailable"})
		return true
	}
	authenticator, okAuthenticator := manager.Authenticator(provider)
	if !okAuthenticator || authenticator == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "provider": provider, "error": "provider_not_found"})
		return true
	}
	cfg := h.runtimeConfigSnapshot()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "provider": provider, "error": "configuration unavailable"})
		return true
	}

	// The login outlives the HTTP request; only request identity is inherited.
	baseCtx := PopulateAuthContext(context.Background(), c)
	session, errCreate := h.plusOAuthStore().createSession(provider, baseCtx)
	if errCreate != nil {
		status := http.StatusInternalServerError
		publicError := "failed to create oauth session"
		switch {
		case errors.Is(errCreate, errPlusOAuthTooManySessions):
			status = http.StatusTooManyRequests
			publicError = "too many active oauth sessions"
		case errors.Is(errCreate, errPlusOAuthClosed):
			status = http.StatusServiceUnavailable
			publicError = "oauth coordinator is closed"
		}
		c.JSON(status, gin.H{"status": "error", "provider": provider, "error": publicError})
		return true
	}

	opts := &sdkAuth.LoginOptions{
		NoBrowser:   true,
		OnChallenge: session.setChallenge,
		Prompt: func(prompt string) (string, error) {
			return session.requestInput(session.loginCtx, prompt)
		},
	}
	if metadata := plusOAuthLoginMetadata(c); len(metadata) > 0 {
		opts.Metadata = metadata
	}
	go h.runPlusOAuthLogin(session, authenticator, cfg, opts)

	c.JSON(http.StatusOK, gin.H{
		"status":   "wait",
		"state":    session.state,
		"provider": provider,
		"flow":     support.Flow,
	})
	return true
}

// runPlusOAuthLogin executes the authenticator directly (never Manager.Login,
// which persists) and saves the record behind the session commit fence.
func (h *Handler) runPlusOAuthLogin(session *plusOAuthSession, authenticator sdkAuth.Authenticator, cfg *config.Config, opts *sdkAuth.LoginOptions) {
	if session == nil || authenticator == nil {
		return
	}
	record, errLogin := authenticator.Login(session.loginCtx, cfg, opts)
	if errLogin != nil {
		if session.loginCtx.Err() != nil {
			session.fail(plusOAuthErrorCancelled)
			return
		}
		// Provider error text can embed tokens, authorization codes, or raw
		// response bodies. Only the stable public category is kept.
		session.fail(plusOAuthErrorAuthFailed)
		return
	}
	if record == nil {
		session.fail(plusOAuthErrorAuthFailed)
		return
	}
	if !session.beginCommit() {
		return
	}
	if _, errSave := h.saveTokenRecord(session.loginCtx, record); errSave != nil {
		// The error value may contain paths or provider identifiers; log the
		// provider category only and expose a stable public message.
		log.WithField("provider", session.provider).Error("plus oauth: failed to save credential")
		session.commitFailed(plusOAuthErrorSaveFailed)
		return
	}
	session.complete()
}

// PostOAuthSessionInput delivers one operator response to a provider prompt.
func (h *Handler) PostOAuthSessionInput(c *gin.Context) {
	var body struct {
		State    string `json:"state"`
		PromptID string `json:"prompt_id"`
		Input    string `json:"input"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, plusOAuthMaxInputBytes)
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid request body"})
		return
	}
	state := strings.TrimSpace(body.State)
	if errState := ValidateOAuthState(state); errState != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	errInput := h.plusOAuthStore().deliverInput(state, strings.TrimSpace(body.PromptID), body.Input)
	switch {
	case errInput == nil:
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	case errors.Is(errInput, errPlusOAuthUnknownSession):
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
	case errors.Is(errInput, errPlusOAuthStalePrompt):
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "prompt response is stale"})
	case errors.Is(errInput, errPlusOAuthNotAwaiting):
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "session is not awaiting input"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to deliver input"})
	}
}

// plusOAuthLoginMetadata forwards a whitelist of non-secret login parameters
// from the start request into LoginOptions. Secrets (personal access tokens,
// OAuth client secrets) are only accepted through the protected prompt input
// endpoint, never from the query string.
func plusOAuthLoginMetadata(c *gin.Context) map[string]string {
	metadata := make(map[string]string)
	for _, pair := range [][2]string{
		{"method", "method"},
		{"flow", "flow"},
		{"start_url", "start-url"},
		{"region", "region"},
		{"base_url", "base_url"},
		{"client_id", "oauth_client_id"},
		{"oauth_client_id", "oauth_client_id"},
		{"login_mode", "login_mode"},
		{"mode", "login_mode"},
	} {
		value := strings.TrimSpace(c.Query(pair[0]))
		if value == "" {
			continue
		}
		if _, exists := metadata[pair[1]]; exists {
			continue
		}
		metadata[pair[1]] = value
	}
	return metadata
}
