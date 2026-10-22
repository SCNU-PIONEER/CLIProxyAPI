package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetKiroQuota fetches usage on demand without starting a background poller.
func (h *Handler) GetKiroQuota(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("AuthIndex"))
	}
	auth := h.findKiroAuth(authIndex)
	if auth == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no kiro credential found"})
		return
	}
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	h.mu.Unlock()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	if proxy := strings.TrimSpace(auth.ProxyURL); proxy != "" {
		cfg.ProxyURL = proxy
	}
	// This bound applies to an explicit management quota request, not inference.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	tokenData := h.ensureKiroTokenFresh(ctx, auth, extractKiroTokenData(auth))
	if tokenData == nil || tokenData.AccessToken == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "kiro token refresh failed"})
		return
	}
	checker := kiro.NewUsageCheckerWithClient(util.SetProxy(&cfg.SDKConfig, &http.Client{}))
	usage, err := checker.CheckUsage(ctx, tokenData)
	if err != nil && (strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "401")) {
		if refreshed := h.forceRefreshKiroToken(ctx, auth); refreshed != nil && refreshed.AccessToken != "" {
			usage, err = checker.CheckUsage(ctx, refreshed)
		}
	}
	if err != nil {
		// Provider bodies and transport errors can contain credentials or PII.
		c.JSON(http.StatusBadGateway, gin.H{"error": "kiro quota request failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"usage": usage, "quota_status": buildKiroQuotaStatus(usage),
		"auth_index": auth.Index, "auth_name": auth.FileName,
	})
}

func (h *Handler) ensureKiroTokenFresh(ctx context.Context, auth *coreauth.Auth, tokenData *kiro.KiroTokenData) *kiro.KiroTokenData {
	if tokenData == nil {
		return nil
	}
	if tokenData.AccessToken == "" {
		return h.forceRefreshKiroToken(ctx, auth)
	}
	if expiresAt, err := time.Parse(time.RFC3339, tokenData.ExpiresAt); err == nil && time.Now().After(expiresAt.Add(-2*time.Minute)) {
		return h.forceRefreshKiroToken(ctx, auth)
	}
	return tokenData
}

// Refresh through the manager so rotated credentials replace shared runtime state
// and retain the normal provider proxy, persistence and generation safeguards.
func (h *Handler) forceRefreshKiroToken(ctx context.Context, auth *coreauth.Auth) *kiro.KiroTokenData {
	if h == nil || h.authManager == nil || auth == nil || ctx.Err() != nil {
		return nil
	}
	refreshed, err := h.authManager.ForceRefreshAuth(ctx, auth.ID)
	if err != nil {
		return nil
	}
	return extractKiroTokenData(refreshed)
}

// findKiroAuth selects an explicit index, or the first active Kiro credential.
func (h *Handler) findKiroAuth(authIndex string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	for _, auth := range h.authManager.List() {
		if auth == nil || auth.Disabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "kiro") {
			continue
		}
		auth.EnsureIndex()
		if authIndex == "" || auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

func extractKiroTokenData(auth *coreauth.Auth) *kiro.KiroTokenData {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	value := func(key string) string {
		text, _ := auth.Metadata[key].(string)
		return text
	}
	if value("access_token") == "" && value("refresh_token") == "" {
		return nil
	}
	return &kiro.KiroTokenData{
		AccessToken: value("access_token"), RefreshToken: value("refresh_token"),
		ProfileArn: value("profile_arn"), ClientID: value("client_id"),
		ClientSecret: value("client_secret"), Region: value("region"),
		StartURL: value("start_url"), ExpiresAt: value("expires_at"),
		AuthMethod: value("auth_method"),
	}
}

func buildKiroQuotaStatus(usage *kiro.UsageQuotaResponse) gin.H {
	if usage == nil {
		return gin.H{"exhausted": true, "remaining": 0}
	}
	status := gin.H{
		"exhausted":        kiro.IsQuotaExhausted(usage),
		"remaining":        kiro.GetRemainingQuota(usage),
		"usage_percentage": kiro.GetUsagePercentage(usage),
	}
	if usage.NextDateReset > 0 {
		status["next_reset"] = time.Unix(int64(usage.NextDateReset/1000), 0)
	}
	if usage.SubscriptionInfo != nil {
		status["subscription"] = usage.SubscriptionInfo
	}
	return status
}
