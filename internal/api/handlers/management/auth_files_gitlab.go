package management

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	gitlabauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gitlab"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

// RequestGitLabPATToken imports a PAT through the same validated login path as the CLI.
// It is mounted only beneath the authenticated v8 management route group.
func (h *Handler) RequestGitLabPATToken(c *gin.Context) {
	var input struct {
		BaseURL             string `json:"base_url"`
		PersonalAccessToken string `json:"personal_access_token"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid GitLab credentials"})
		return
	}
	input.PersonalAccessToken = strings.TrimSpace(input.PersonalAccessToken)
	if input.PersonalAccessToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "personal_access_token is required"})
		return
	}
	base := gitlabauth.NormalizeBaseURL(input.BaseURL)
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "base_url must be an HTTPS GitLab instance URL"})
		return
	}
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	h.mu.Unlock()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	record, err := sdkauth.NewGitLabAuthenticator().Login(c.Request.Context(), cfg, &sdkauth.LoginOptions{
		NoBrowser: true,
		Metadata: map[string]string{
			"login_mode": "pat", "base_url": base,
			"personal_access_token": input.PersonalAccessToken,
		},
	})
	if err != nil {
		// Provider response bodies can contain credentials; never reflect login errors.
		c.JSON(http.StatusBadGateway, gin.H{"error": "GitLab authentication failed"})
		return
	}
	if _, err = h.saveTokenRecord(c.Request.Context(), record); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist GitLab credentials"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "provider": "gitlab",
		"model_provider": record.Metadata["model_provider"],
		"model_name":     record.Metadata["model_name"],
	})
}
